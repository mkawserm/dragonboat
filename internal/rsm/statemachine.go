// Copyright 2017-2019 Lei Ni (nilei81@gmail.com) and other Dragonboat authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package rsm implements Replicated State Machines used in Dragonboat.

This package is internally used by Dragonboat, applications are not expected to
import this package.
*/
package rsm

import (
	"bytes"
	"errors"
	"sync"

	"github.com/lni/goutils/logutil"
	"github.com/mkawserm/dragonboat/v3/config"
	"github.com/mkawserm/dragonboat/v3/internal/raft"
	"github.com/mkawserm/dragonboat/v3/internal/server"
	"github.com/mkawserm/dragonboat/v3/internal/settings"
	"github.com/mkawserm/dragonboat/v3/internal/tests"
	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/logger"
	pb "github.com/mkawserm/dragonboat/v3/raftpb"
	sm "github.com/mkawserm/dragonboat/v3/statemachine"
)

var (
	plog = logger.GetLogger("rsm")
)

var (
	// ErrTestKnobReturn is the error returned when returned earlier due to test
	// knob.
	ErrTestKnobReturn = errors.New("returned earlier due to test knob")
	// ErrRestoreSnapshot indicates there is error when trying to restore
	// from a snapshot
	ErrRestoreSnapshot             = errors.New("failed to restore from snapshot")
	batchedEntryApply              = settings.Soft.BatchedEntryApply
	sessionBufferInitialCap uint64 = 128 * 1024
)

// SSReqType is the type of a snapshot request.
type SSReqType uint64

const (
	// Periodic is the value to indicate periodic snapshot.
	Periodic SSReqType = iota
	// UserRequested is the value to indicate user requested snapshot.
	UserRequested
	// Exported is the value to indicate exported snapshot.
	Exported
	// Streaming is the value to indicate snapshot streaming.
	Streaming
)

// SSRequest is the type for describing the details of a snapshot request.
type SSRequest struct {
	Type               SSReqType
	Key                uint64
	Path               string
	OverrideCompaction bool
	CompactionOverhead uint64
}

// Exported returns a boolean value indicating whether the snapshot request
// is to create an exported snapshot.
func (r *SSRequest) Exported() bool {
	return r.Type == Exported
}

// Streaming returns a boolean value indicating whether the snapshot request
// is to stream snapshot.
func (r *SSRequest) Streaming() bool {
	return r.Type == Streaming
}

// SSMeta is the metadata of a snapshot.
type SSMeta struct {
	From            uint64
	Index           uint64
	Term            uint64
	OnDiskIndex     uint64 // applied index of IOnDiskStateMachine
	Request         SSRequest
	Membership      pb.Membership
	Type            pb.StateMachineType
	Session         *bytes.Buffer
	Ctx             interface{}
	CompressionType config.CompressionType
}

// Task describes a task that need to be handled by StateMachine.
type Task struct {
	ClusterID         uint64
	NodeID            uint64
	Index             uint64
	SnapshotAvailable bool
	InitialSnapshot   bool
	SnapshotRequested bool
	StreamSnapshot    bool
	PeriodicSync      bool
	NewNode           bool
	SSRequest         SSRequest
	Entries           []pb.Entry
}

// IsSnapshotTask returns a boolean flag indicating whether it is a snapshot
// task.
func (t *Task) IsSnapshotTask() bool {
	return t.SnapshotAvailable || t.SnapshotRequested || t.StreamSnapshot
}

func (t *Task) isSyncTask() bool {
	if t.PeriodicSync && t.IsSnapshotTask() {
		panic("invalid task")
	}
	return t.PeriodicSync
}

// SMFactoryFunc is the function type for creating an IStateMachine instance
type SMFactoryFunc func(clusterID uint64,
	nodeID uint64, done <-chan struct{}) IManagedStateMachine

// INode is the interface of a dragonboat node.
type INode interface {
	StepReady()
	RestoreRemotes(pb.Snapshot)
	ApplyUpdate(pb.Entry, sm.Result, bool, bool, bool)
	ApplyConfigChange(pb.ConfigChange)
	ConfigChangeProcessed(uint64, bool)
	NodeID() uint64
	ClusterID() uint64
	ShouldStop() <-chan struct{}
}

// ISnapshotter is the interface for the snapshotter object.
type ISnapshotter interface {
	GetSnapshot(uint64) (pb.Snapshot, error)
	GetMostRecentSnapshot() (pb.Snapshot, error)
	GetFilePath(uint64) string
	Stream(IStreamable, *SSMeta, pb.IChunkSink) error
	Save(ISavable, *SSMeta) (*pb.Snapshot, *server.SSEnv, error)
	Load(ILoadable, IRecoverable, string, []sm.SnapshotFile) error
	IsNoSnapshotError(error) bool
}

// StateMachine is a manager class that manages application state
// machine
type StateMachine struct {
	mu              sync.RWMutex
	snapshotter     ISnapshotter
	node            INode
	sm              IManagedStateMachine
	sessions        *SessionManager
	members         *membership
	index           uint64
	term            uint64
	snapshotIndex   uint64
	onDiskInitIndex uint64
	onDiskIndex     uint64
	taskQ           *TaskQueue
	onDiskSM        bool
	aborted         bool
	isWitness       bool
	sct             config.CompressionType
	fs              vfs.IFS
	syncedIndex     struct {
		sync.Mutex
		index uint64
	}
	batchedIndex struct {
		sync.Mutex
		index uint64
	}
}

// NewStateMachine creates a new application state machine object.
func NewStateMachine(sm IManagedStateMachine,
	snapshotter ISnapshotter,
	cfg config.Config, node INode, fs vfs.IFS) *StateMachine {
	ordered := cfg.OrderedConfigChange
	return &StateMachine{
		snapshotter: snapshotter,
		sm:          sm,
		onDiskSM:    sm.OnDisk(),
		taskQ:       NewTaskQueue(),
		node:        node,
		sessions:    NewSessionManager(),
		members:     newMembership(node.ClusterID(), node.NodeID(), ordered),
		isWitness:   cfg.IsWitness,
		sct:         cfg.SnapshotCompressionType,
		fs:          fs,
	}
}

// TaskQ returns the task queue.
func (s *StateMachine) TaskQ() *TaskQueue {
	return s.taskQ
}

// TaskChanBusy returns whether the TaskC chan is busy. Busy is defined as
// having more than half of its buffer occupied.
func (s *StateMachine) TaskChanBusy() bool {
	sz := s.taskQ.Size()
	return sz*2 > taskQueueBusyCap
}

// DestroyedC return a chan struct{} used to indicate whether the SM has been
// fully unloaded.
func (s *StateMachine) DestroyedC() <-chan struct{} {
	return s.sm.DestroyedC()
}

// RecoverFromSnapshot applies the snapshot.
func (s *StateMachine) RecoverFromSnapshot(t Task) (uint64, error) {
	ss, err := s.getSnapshot(t)
	if err != nil {
		return 0, err
	}
	if pb.IsEmptySnapshot(ss) {
		return 0, nil
	}
	ss.Validate(s.fs)
	plog.Infof("%s called RecoverFromSnapshot, %s, on disk idx %d",
		s.id(), s.ssid(ss.Index), ss.OnDiskIndex)
	if err := s.recoverFromSnapshot(ss, t.InitialSnapshot); err != nil {
		return 0, err
	}
	s.node.RestoreRemotes(ss)
	s.setBatchedLastApplied(ss.Index)
	plog.Infof("%s restored %s", s.id(), s.ssid(ss.Index))
	return ss.Index, nil
}

func (s *StateMachine) getSnapshot(t Task) (pb.Snapshot, error) {
	if !t.InitialSnapshot {
		snapshot, err := s.snapshotter.GetSnapshot(t.Index)
		if err != nil && !s.snapshotter.IsNoSnapshotError(err) {
			plog.Errorf("%s, get snapshot failed: %v", s.id(), err)
			return pb.Snapshot{}, ErrRestoreSnapshot
		}
		if s.snapshotter.IsNoSnapshotError(err) {
			plog.Errorf("%s, no snapshot", s.id())
			return pb.Snapshot{}, err
		}
		return snapshot, nil
	}
	snapshot, err := s.snapshotter.GetMostRecentSnapshot()
	if s.snapshotter.IsNoSnapshotError(err) {
		plog.Infof("%s no snapshot available during launch", s.id())
		return pb.Snapshot{}, nil
	}
	return snapshot, nil
}

func (s *StateMachine) mustBeOnDiskSM() {
	if !s.OnDiskStateMachine() {
		panic("not an on disk sm")
	}
}

func (s *StateMachine) recoverSMRequired(ss pb.Snapshot, init bool) bool {
	s.mustBeOnDiskSM()
	fn := s.snapshotter.GetFilePath(ss.Index)
	shrunk, err := IsShrinkedSnapshotFile(fn, s.fs)
	if err != nil {
		panic(err)
	}
	if !init && shrunk {
		panic("not initial recovery but snapshot shrunk")
	}
	if init {
		if shrunk {
			return false
		}
		if ss.Imported {
			return true
		}
		return ss.OnDiskIndex > s.onDiskInitIndex
	}
	return ss.OnDiskIndex > s.onDiskIndex
}

func (s *StateMachine) canRecoverOnDiskSnapshot(ss pb.Snapshot, init bool) {
	s.mustBeOnDiskSM()
	if ss.Imported && init {
		return
	}
	if ss.OnDiskIndex <= s.onDiskInitIndex {
		plog.Panicf("%s, ss.OnDiskIndex (%d) <= s.onDiskInitIndex (%d)",
			s.id(), ss.OnDiskIndex, s.onDiskInitIndex)
	}
	if ss.OnDiskIndex <= s.onDiskIndex {
		plog.Panicf("%s, ss.OnDiskInit (%d) <= s.onDiskIndex (%d)",
			s.id(), ss.OnDiskIndex, s.onDiskIndex)
	}
}

func (s *StateMachine) recoverFromSnapshot(ss pb.Snapshot, init bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := ss.Index
	if s.index >= index {
		return raft.ErrSnapshotOutOfDate
	}
	if s.aborted {
		return sm.ErrSnapshotStopped
	}
	if ss.Witness || ss.Dummy {
		s.applySnapshot(ss)
		return nil
	}
	if !s.OnDiskStateMachine() {
		return s.recover(ss, init)
	}
	if s.recoverSMRequired(ss, init) {
		s.canRecoverOnDiskSnapshot(ss, init)
		if err := s.recover(ss, init); err != nil {
			return err
		}
		s.applyOnDiskSnapshot(ss, init)
	} else {
		plog.Infof("%s is on disk SM, %d vs %d, SM not restored",
			s.id(), index, s.onDiskInitIndex)
		s.applySnapshot(ss)
	}
	return nil
}

func (s *StateMachine) recover(ss pb.Snapshot, init bool) error {
	index := ss.Index
	plog.Infof("%s recovering from %s, init %t", s.id(), s.ssid(index), init)
	s.logMembership("members", index, ss.Membership.Addresses)
	s.logMembership("observers", index, ss.Membership.Observers)
	s.logMembership("witnesses", index, ss.Membership.Witnesses)
	fs := make([]sm.SnapshotFile, 0)
	for _, f := range ss.Files {
		f := sm.SnapshotFile{
			FileID:   f.FileId,
			Filepath: f.Filepath,
			Metadata: f.Metadata,
		}
		fs = append(fs, f)
	}
	fn := s.snapshotter.GetFilePath(index)
	if err := s.snapshotter.Load(s.sessions, s.sm, fn, fs); err != nil {
		plog.Errorf("%s failed to load %s, %v", s.id(), s.ssid(index), err)
		if err == sm.ErrSnapshotStopped {
			s.aborted = true
			return err
		}
		return ErrRestoreSnapshot
	}
	s.applySnapshot(ss)
	return nil
}

func (s *StateMachine) applySnapshot(ss pb.Snapshot) {
	s.index = ss.Index
	s.term = ss.Term
	s.members.set(ss.Membership)
}

func (s *StateMachine) applyOnDiskSnapshot(ss pb.Snapshot, init bool) {
	s.mustBeOnDiskSM()
	s.onDiskIndex = ss.OnDiskIndex
	if ss.Imported && init {
		s.onDiskInitIndex = ss.OnDiskIndex
	}
	s.applySnapshot(ss)
}

//TODO: add test to cover the case when ReadyToStreamSnapshot returns false

// ReadyToStreamSnapshot returns a boolean flag to indicate whether the state
// machine is ready to stream snapshot. It can not stream a full snapshot when
// membership state is catching up with the all disk SM state. however, meta
// only snapshot can be taken at any time.
func (s *StateMachine) ReadyToStreamSnapshot() bool {
	if !s.OnDiskStateMachine() {
		return true
	}
	return s.GetLastApplied() >= s.onDiskInitIndex
}

func (s *StateMachine) tryInjectTestFS() {
	nsm, ok := s.sm.(*NativeSM)
	if ok {
		odsm, ok := nsm.sm.(*OnDiskStateMachine)
		if ok {
			odsm.SetTestFS(s.fs)
		}
	}
}

// OpenOnDiskStateMachine opens the on disk state machine.
func (s *StateMachine) OpenOnDiskStateMachine() (uint64, error) {
	s.mustBeOnDiskSM()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tryInjectTestFS()
	index, err := s.sm.Open()
	if err != nil {
		plog.Errorf("%s failed to open on disk SM", s.id())
		if err == sm.ErrOpenStopped {
			s.aborted = true
		}
		return 0, err
	}
	plog.Infof("%s opened disk SM, index %d", s.id(), index)
	s.onDiskInitIndex = index
	s.onDiskIndex = index
	return index, nil
}

// GetLastApplied returns the last applied value.
func (s *StateMachine) GetLastApplied() uint64 {
	s.mu.RLock()
	v := s.index
	s.mu.RUnlock()
	return v
}

// GetBatchedLastApplied returns the batched last applied value.
func (s *StateMachine) GetBatchedLastApplied() uint64 {
	s.batchedIndex.Lock()
	v := s.batchedIndex.index
	s.batchedIndex.Unlock()
	return v
}

// GetSyncedIndex returns the index value that is known to have been
// synchronized.
func (s *StateMachine) GetSyncedIndex() uint64 {
	s.syncedIndex.Lock()
	defer s.syncedIndex.Unlock()
	return s.syncedIndex.index
}

// SetBatchedLastApplied sets the batched last applied value. This method
// is mostly used in tests.
func (s *StateMachine) SetBatchedLastApplied(index uint64) {
	s.setBatchedLastApplied(index)
}

func (s *StateMachine) setSyncedIndex(index uint64) {
	s.syncedIndex.Lock()
	defer s.syncedIndex.Unlock()
	if s.syncedIndex.index > index {
		panic("s.syncedIndex.index > index")
	}
	s.syncedIndex.index = index
}

func (s *StateMachine) setBatchedLastApplied(index uint64) {
	s.batchedIndex.Lock()
	s.batchedIndex.index = index
	s.batchedIndex.Unlock()
}

// Offloaded marks the state machine as offloaded from the specified compone.
// It returns a boolean value indicating whether the node has been fully
// unloaded after unloading from the specified compone.
func (s *StateMachine) Offloaded(from From) bool {
	return s.sm.Offloaded(from)
}

// Loaded marks the state machine as loaded from the specified compone.
func (s *StateMachine) Loaded(from From) {
	s.sm.Loaded(from)
}

// Lookup queries the local state machine.
func (s *StateMachine) Lookup(query interface{}) (interface{}, error) {
	if s.Concurrent() {
		return s.concurrentLookup(query)
	}
	return s.lookup(query)
}

func (s *StateMachine) lookup(query interface{}) (interface{}, error) {
	s.mu.RLock()
	if s.aborted {
		s.mu.RUnlock()
		return nil, ErrClusterClosed
	}
	result, err := s.sm.Lookup(query)
	s.mu.RUnlock()
	return result, err
}

func (s *StateMachine) concurrentLookup(query interface{}) (interface{}, error) {
	return s.sm.Lookup(query)
}

// NALookup queries the local state machine.
func (s *StateMachine) NALookup(query []byte) ([]byte, error) {
	if s.Concurrent() {
		return s.naConcurrentLookup(query)
	}
	return s.nalookup(query)
}

func (s *StateMachine) nalookup(query []byte) ([]byte, error) {
	s.mu.RLock()
	if s.aborted {
		s.mu.RUnlock()
		return nil, ErrClusterClosed
	}
	result, err := s.sm.NALookup(query)
	s.mu.RUnlock()
	return result, err
}

func (s *StateMachine) naConcurrentLookup(query []byte) ([]byte, error) {
	return s.sm.NALookup(query)
}

// GetMembership returns the membership info maintained by the state machine.
func (s *StateMachine) GetMembership() pb.Membership {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.members.get()
}

// Concurrent returns a boolean flag indicating whether the state machine is
// capable of taking concurrent snapshot.
func (s *StateMachine) Concurrent() bool {
	return s.sm.Concurrent()
}

// OnDiskStateMachine returns a boolean flag indicating whether it is an on
// disk state machine.
func (s *StateMachine) OnDiskStateMachine() bool {
	return s.onDiskSM && !s.isWitness
}

// SaveSnapshot creates a snapshot.
func (s *StateMachine) SaveSnapshot(req SSRequest) (*pb.Snapshot,
	*server.SSEnv, error) {
	if req.Streaming() {
		panic("invalid snapshot request")
	}
	if s.isWitness {
		plog.Panicf("witness %s saving snapshot", s.id())
	}
	if s.Concurrent() {
		return s.saveConcurrentSnapshot(req)
	}
	return s.saveSnapshot(req)
}

// StreamSnapshot starts to stream snapshot from the current SM to a remote
// node targeted by the provided sink.
func (s *StateMachine) StreamSnapshot(sink pb.IChunkSink) error {
	return s.streamSnapshot(sink)
}

// Sync synchronizes state machine's in-core state with that on disk.
func (s *StateMachine) Sync() error {
	return s.sync()
}

// GetHash returns the state machine hash.
func (s *StateMachine) GetHash() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sm.GetHash()
}

// GetSessionHash returns the session hash.
func (s *StateMachine) GetSessionHash() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions.GetSessionHash()
}

// GetMembershipHash returns the hash of the membership instance.
func (s *StateMachine) GetMembershipHash() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.members.getHash()
}

// Handle pulls the committed record and apply it if there is any available.
func (s *StateMachine) Handle(batch []Task, apply []sm.Entry) (Task, error) {
	batch = batch[:0]
	apply = apply[:0]
	processed := false
	defer func() {
		// give the node worker a chance to run when
		//  - batched applied value has been updated
		//  - taskC has been popped
		if processed {
			s.node.StepReady()
		}
	}()
	rec, ok := s.taskQ.Get()
	if ok {
		processed = true
		if rec.IsSnapshotTask() {
			return rec, nil
		}
		if !rec.isSyncTask() {
			batch = append(batch, rec)
		} else {
			if err := s.sync(); err != nil {
				return Task{}, err
			}
		}
		done := false
		for !done {
			rec, ok := s.taskQ.Get()
			if ok {
				if rec.IsSnapshotTask() {
					if err := s.handle(batch, apply); err != nil {
						return Task{}, err
					}
					return rec, nil
				}
				if !rec.isSyncTask() {
					batch = append(batch, rec)
				} else {
					if err := s.sync(); err != nil {
						return Task{}, err
					}
				}
			} else {
				done = true
			}
		}
	}
	return Task{}, s.handle(batch, apply)
}

func (s *StateMachine) isDummySnapshot(r SSRequest) bool {
	return s.OnDiskStateMachine() && !r.Exported() && !r.Streaming()
}

func (s *StateMachine) logMembership(name string,
	index uint64, members map[uint64]string) {
	plog.Infof("%d %s included in %s", len(members), name, s.ssid(index))
	for nid, addr := range members {
		plog.Infof("\t%s : %s", logutil.NodeID(nid), addr)
	}
}

func (s *StateMachine) getSSMeta(c interface{}, r SSRequest) (*SSMeta, error) {
	if s.members.isEmpty() {
		plog.Panicf("%s has empty membership", s.id())
	}
	ct := s.sct
	// never compress dummy snapshot file
	if s.isDummySnapshot(r) {
		ct = config.NoCompression
	}
	buf := bytes.NewBuffer(make([]byte, 0, sessionBufferInitialCap))
	meta := &SSMeta{
		From:            s.node.NodeID(),
		Ctx:             c,
		Index:           s.index,
		Term:            s.term,
		OnDiskIndex:     s.onDiskIndex,
		Request:         r,
		Session:         buf,
		Membership:      s.members.get(),
		Type:            s.sm.Type(),
		CompressionType: ct,
	}
	plog.Infof("%s generating %s", s.id(), s.ssid(meta.Index))
	s.logMembership("members", meta.Index, meta.Membership.Addresses)
	if err := s.sessions.SaveSessions(meta.Session); err != nil {
		return nil, err
	}
	return meta, nil
}

func (s *StateMachine) updateLastApplied(index uint64, term uint64) {
	if s.index+1 != index {
		plog.Panicf("%s invalid index %d, applied %d", s.id(), index, s.index)
	}
	if index == 0 || term == 0 {
		plog.Panicf("%s invalid index %d or term %d", s.id(), index, term)
	}
	if term < s.term {
		plog.Panicf("%s term %d moving backward, new term %d", s.id(), s.term, term)
	}
	s.index = index
	s.term = term
}

func (s *StateMachine) checkSnapshotStatus(req SSRequest) error {
	if s.aborted {
		return sm.ErrSnapshotStopped
	}
	if s.index < s.snapshotIndex {
		panic("s.index < s.snapshotIndex")
	}
	if !s.OnDiskStateMachine() {
		if !req.Exported() && s.index > 0 && s.index == s.snapshotIndex {
			return raft.ErrSnapshotOutOfDate
		}
	}
	return nil
}

func (s *StateMachine) streamSnapshot(sink pb.IChunkSink) error {
	var err error
	var meta *SSMeta
	if err := func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		meta, err = s.prepareSnapshot(SSRequest{Type: Streaming})
		return err
	}(); err != nil {
		return err
	}
	if tests.ReadyToReturnTestKnob(s.node.ShouldStop(), "snapshotter.Stream") {
		return ErrTestKnobReturn
	}
	return s.snapshotter.Stream(s.sm, meta, sink)
}

func (s *StateMachine) saveConcurrentSnapshot(req SSRequest) (*pb.Snapshot,
	*server.SSEnv, error) {
	var err error
	var meta *SSMeta
	if err := func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		meta, err = s.prepareSnapshot(req)
		return err
	}(); err != nil {
		return nil, nil, err
	}
	if tests.ReadyToReturnTestKnob(s.node.ShouldStop(), "s.sync") {
		return nil, nil, ErrTestKnobReturn
	}
	if err := s.sync(); err != nil {
		return nil, nil, err
	}
	if tests.ReadyToReturnTestKnob(s.node.ShouldStop(), "s.doSaveSnapshot") {
		return nil, nil, ErrTestKnobReturn
	}
	return s.doSaveSnapshot(meta)
}

func (s *StateMachine) saveSnapshot(req SSRequest) (*pb.Snapshot,
	*server.SSEnv, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, err := s.prepareSnapshot(req)
	if err != nil {
		plog.Errorf("prepare snapshot failed %v", err)
		return nil, nil, err
	}
	if tests.ReadyToReturnTestKnob(s.node.ShouldStop(), "s.doSaveSnapshot") {
		return nil, nil, ErrTestKnobReturn
	}
	return s.doSaveSnapshot(meta)
}

func (s *StateMachine) prepareSnapshot(req SSRequest) (*SSMeta, error) {
	if err := s.checkSnapshotStatus(req); err != nil {
		return nil, err
	}
	var err error
	var ctx interface{}
	if s.Concurrent() {
		ctx, err = s.sm.Prepare()
		if err != nil {
			return nil, err
		}
	}
	return s.getSSMeta(ctx, req)
}

func (s *StateMachine) sync() error {
	if !s.OnDiskStateMachine() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plog.Infof("%s is being synchronized, index %d", s.id(), s.index)
	if err := s.sm.Sync(); err != nil {
		return err
	}
	s.setSyncedIndex(s.index)
	return nil
}

func (s *StateMachine) doSaveSnapshot(meta *SSMeta) (*pb.Snapshot,
	*server.SSEnv, error) {
	snapshot, env, err := s.snapshotter.Save(s.sm, meta)
	if err != nil {
		plog.Errorf("%s snapshotter.Save failed %v", s.id(), err)
		return nil, env, err
	}
	s.snapshotIndex = meta.Index
	return snapshot, env, nil
}

func getEntryTypes(entries []pb.Entry) (bool, bool) {
	allUpdate := true
	allNoOP := true
	for _, v := range entries {
		if allUpdate && !v.IsUpdateEntry() {
			allUpdate = false
		}
		if allNoOP && !v.IsNoOPSession() {
			allNoOP = false
		}
	}
	return allUpdate, allNoOP
}

func (s *StateMachine) handle(batch []Task, toApply []sm.Entry) error {
	batchSupport := batchedEntryApply && s.Concurrent()
	for b := range batch {
		if batch[b].IsSnapshotTask() || batch[b].isSyncTask() {
			plog.Panicf("%s trying to handle a snapshot/sync request", s.id())
		}
		input := batch[b].Entries
		allUpdate, allNoOP := getEntryTypes(input)
		if batchSupport && allUpdate && allNoOP {
			if err := s.handleBatch(input, toApply); err != nil {
				return err
			}
		} else {
			for i := range input {
				last := b == len(batch)-1 && i == len(input)-1
				if err := s.handleEntry(input[i], last); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isEmptyResult(result sm.Result) bool {
	return result.Data == nil && result.Value == 0
}

func (s *StateMachine) entryInInitDiskSM(index uint64) bool {
	if !s.OnDiskStateMachine() {
		return false
	}
	return index <= s.onDiskInitIndex
}

func (s *StateMachine) updateOnDiskIndex(firstIndex uint64, lastIndex uint64) {
	if !s.OnDiskStateMachine() {
		return
	}
	if firstIndex > lastIndex {
		panic("firstIndex > lastIndex")
	}
	if firstIndex <= s.onDiskInitIndex {
		plog.Panicf("last entry index to apply %d, initial on disk index %d",
			firstIndex, s.onDiskInitIndex)
	}
	if firstIndex <= s.onDiskIndex {
		plog.Panicf("last entry index to apply %d, on disk index %d",
			firstIndex, s.onDiskIndex)
	}
	s.onDiskIndex = lastIndex
}

func (s *StateMachine) handleEntry(e pb.Entry, last bool) error {
	if e.IsConfigChange() {
		accepted := s.handleConfigChange(e)
		s.node.ConfigChangeProcessed(e.Key, accepted)
	} else {
		if !e.IsSessionManaged() {
			if e.IsEmpty() {
				s.handleNoOP(e)
				s.node.ApplyUpdate(e, sm.Result{}, false, true, last)
			} else {
				panic("not session managed, not empty")
			}
		} else {
			if e.IsNewSessionRequest() {
				smResult := s.handleRegisterSession(e)
				s.node.ApplyUpdate(e, smResult, isEmptyResult(smResult), false, last)
			} else if e.IsEndOfSessionRequest() {
				smResult := s.handleUnregisterSession(e)
				s.node.ApplyUpdate(e, smResult, isEmptyResult(smResult), false, last)
			} else {
				if !s.entryInInitDiskSM(e.Index) {
					smResult, ignored, rejected, err := s.handleUpdate(e)
					if err != nil {
						return err
					}
					if !ignored {
						s.node.ApplyUpdate(e, smResult, rejected, ignored, last)
					}
				} else {
					// treat it as a NoOP entry
					s.handleNoOP(pb.Entry{Index: e.Index, Term: e.Term})
				}
			}
		}
	}
	index := s.GetLastApplied()
	if index != e.Index {
		plog.Panicf("unexpected last applied value, %d, %d", index, e.Index)
	}
	if last {
		s.setBatchedLastApplied(e.Index)
	}
	return nil
}

func (s *StateMachine) onUpdateApplied(e pb.Entry,
	result sm.Result, ignored bool, rejected bool, last bool) {
	if !ignored {
		s.node.ApplyUpdate(e, result, rejected, ignored, last)
	}
}

func (s *StateMachine) handleBatch(input []pb.Entry, ents []sm.Entry) error {
	if len(ents) != 0 {
		panic("ents is not empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	skipped := 0
	for _, e := range input {
		if !s.entryInInitDiskSM(e.Index) {
			rec := sm.Entry{
				Index: e.Index,
				Cmd:   getEntryPayload(e),
			}
			ents = append(ents, rec)
		} else {
			skipped++
		}
		s.updateLastApplied(e.Index, e.Term)
	}
	if len(ents) > 0 {
		firstIndex := ents[0].Index
		lastIndex := ents[len(ents)-1].Index
		s.updateOnDiskIndex(firstIndex, lastIndex)
		results, err := s.sm.BatchedUpdate(ents)
		if err != nil {
			return err
		}
		for idx, e := range results {
			ce := input[skipped+idx]
			if ce.Index != e.Index {
				// probably because user modified the Index value in results
				plog.Panicf("%s alignment error, %d, %d, %d",
					s.id(), ce.Index, e.Index, skipped)
			}
			last := ce.Index == input[len(input)-1].Index
			s.onUpdateApplied(ce, e.Result, false, false, last)
		}
	}
	if len(input) > 0 {
		s.setBatchedLastApplied(input[len(input)-1].Index)
	}
	return nil
}

func (s *StateMachine) handleConfigChange(e pb.Entry) bool {
	var cc pb.ConfigChange
	if err := cc.Unmarshal(e.Cmd); err != nil {
		panic(err)
	}
	if cc.Type == pb.AddNode && len(cc.Address) == 0 {
		panic("empty address in AddNode request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateLastApplied(e.Index, e.Term)
	if s.members.handleConfigChange(cc, e.Index) {
		s.node.ApplyConfigChange(cc)
		return true
	}
	return false
}

func (s *StateMachine) handleRegisterSession(e pb.Entry) sm.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	smResult := s.sessions.RegisterClientID(e.ClientID)
	if isEmptyResult(smResult) {
		plog.Errorf("%s register client failed %v", s.id(), e)
	}
	s.updateLastApplied(e.Index, e.Term)
	return smResult
}

func (s *StateMachine) handleUnregisterSession(e pb.Entry) sm.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	smResult := s.sessions.UnregisterClientID(e.ClientID)
	if isEmptyResult(smResult) {
		plog.Errorf("%s unregister %d failed %v", s.id(), e.ClientID, e)
	}
	s.updateLastApplied(e.Index, e.Term)
	return smResult
}

func (s *StateMachine) handleNoOP(e pb.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !e.IsEmpty() || e.IsSessionManaged() {
		panic("handle empty event called on non-empty event")
	}
	s.updateLastApplied(e.Index, e.Term)
}

// result a tuple of (result, should ignore, rejected)
func (s *StateMachine) handleUpdate(e pb.Entry) (sm.Result, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ok bool
	var session *Session
	s.updateLastApplied(e.Index, e.Term)
	if !e.IsNoOPSession() {
		session, ok = s.sessions.ClientRegistered(e.ClientID)
		if !ok {
			// client is expected to crash
			return sm.Result{}, false, true, nil
		}
		s.sessions.UpdateRespondedTo(session, e.RespondedTo)
		v, responded, toUpdate := s.sessions.UpdateRequired(session, e.SeriesID)
		if responded {
			// should ignore. client is expected to timeout
			return sm.Result{}, true, false, nil
		}
		if !toUpdate {
			// server responded, client never confirmed
			// return the result again but not update the sm again
			// this implements the no-more-than-once update of the SM
			return v, false, false, nil
		}
	}
	if !e.IsNoOPSession() && session == nil {
		panic("session not found")
	}
	if session != nil {
		if _, ok := session.getResponse(RaftSeriesID(e.SeriesID)); ok {
			panic("already has response in session")
		}
	}
	s.updateOnDiskIndex(e.Index, e.Index)
	cmd := getEntryPayload(e)
	result, err := s.sm.Update(sm.Entry{Index: e.Index, Cmd: cmd})
	if err != nil {
		return sm.Result{}, false, false, err
	}
	if session != nil {
		session.addResponse(RaftSeriesID(e.SeriesID), result)
	}
	return result, false, false, nil
}

func (s *StateMachine) id() string {
	return logutil.DescribeSM(s.node.ClusterID(), s.node.NodeID())
}

func (s *StateMachine) ssid(index uint64) string {
	return logutil.DescribeSS(s.node.ClusterID(), s.node.NodeID(), index)
}
