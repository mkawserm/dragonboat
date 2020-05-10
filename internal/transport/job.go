// Copyright 2017-2019 Lei Ni (nilei81@gmaij.com) and other Dragonboat authors.
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

package transport

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/lni/goutils/logutil"

	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/raftio"
	pb "github.com/mkawserm/dragonboat/v3/raftpb"
)

const (
	streamingChanLength = 16
)

var (
	// ErrStopped is the error returned to indicate that the connection has
	// already been stopped.
	ErrStopped = errors.New("connection stopped")
	// ErrStreamSnapshot is the error returned to indicate that snapshot
	// streaming failed.
	ErrStreamSnapshot = errors.New("stream snapshot failed")
)

// Sink is the chunk sink for receiving generated snapshot chunk.
type Sink struct {
	j *job
}

// Receive receives a snapshot chunk.
func (s *Sink) Receive(chunk pb.Chunk) (bool, bool) {
	return s.j.SendChunk(chunk)
}

// Stop stops the sink processing.
func (s *Sink) Stop() {
	s.Receive(pb.Chunk{ChunkCount: pb.PoisonChunkCount})
}

// ClusterID returns the cluster ID of the source node.
func (s *Sink) ClusterID() uint64 {
	return s.j.clusterID
}

// ToNodeID returns the node ID of the node intended to get and handle the
// received snapshot chunk.
func (s *Sink) ToNodeID() uint64 {
	return s.j.nodeID
}

type job struct {
	clusterID          uint64
	nodeID             uint64
	deploymentID       uint64
	streaming          bool
	ctx                context.Context
	rpc                raftio.IRaftRPC
	conn               raftio.ISnapshotConnection
	ch                 chan pb.Chunk
	stopc              chan struct{}
	failed             chan struct{}
	streamChunkSent    atomic.Value
	preStreamChunkSend atomic.Value
	fs                 vfs.IFS
}

func newJob(ctx context.Context,
	clusterID uint64, nodeID uint64,
	did uint64, streaming bool, sz int, rpc raftio.IRaftRPC,
	stopc chan struct{}, fs vfs.IFS) *job {
	j := &job{
		clusterID:    clusterID,
		nodeID:       nodeID,
		deploymentID: did,
		streaming:    streaming,
		ctx:          ctx,
		rpc:          rpc,
		stopc:        stopc,
		failed:       make(chan struct{}),
		fs:           fs,
	}
	var chsz int
	if streaming {
		chsz = streamingChanLength
	} else {
		chsz = sz
	}
	j.ch = make(chan pb.Chunk, chsz)
	return j
}

func (j *job) close() {
	if j.conn != nil {
		j.conn.Close()
	}
}

func (j *job) connect(addr string) error {
	conn, err := j.rpc.GetSnapshotConnection(j.ctx, addr)
	if err != nil {
		plog.Errorf("failed to get a job to %s, %v", addr, err)
		return err
	}
	j.conn = conn
	return nil
}

func (j *job) sendSavedSnapshot(m pb.Message) {
	chunks := splitSnapshotMessage(m, j.fs)
	if len(chunks) != cap(j.ch) {
		plog.Panicf("cap of ch is %d, want %d", cap(j.ch), len(chunks))
	}
	for _, chunk := range chunks {
		j.ch <- chunk
	}
}

func (j *job) SendChunk(chunk pb.Chunk) (bool, bool) {
	if !chunk.IsPoisonChunk() {
		plog.Infof("%s is sending chunk %d to %s",
			logutil.NodeID(chunk.From), chunk.ChunkId,
			dn(chunk.ClusterId, chunk.NodeId))
	} else {
		plog.Infof("sending a poison chunk to %s", dn(j.clusterID, j.nodeID))
	}

	select {
	case j.ch <- chunk:
		return true, false
	case <-j.failed:
		plog.Infof("stream snapshot to %s failed", dn(j.clusterID, j.nodeID))
		return false, false
	case <-j.stopc:
		return false, true
	}
}

func (j *job) process() error {
	if j.conn == nil {
		panic("trying to process on nil ch, not connected?")
	}
	if j.streaming {
		err := j.streamSnapshot()
		if err != nil {
			close(j.failed)
		}
		return err
	}
	return j.processSavedSnapshot()
}

func (j *job) streamSnapshot() error {
	for {
		select {
		case <-j.stopc:
			plog.Infof("stream snapshot to %s stopped", dn(j.clusterID, j.nodeID))
			return ErrStopped
		case chunk := <-j.ch:
			chunk.DeploymentId = j.deploymentID
			if chunk.IsPoisonChunk() {
				return ErrStreamSnapshot
			}
			if err := j.sendChunk(chunk, j.conn); err != nil {
				plog.Errorf("streaming snapshot chunk to %s failed",
					dn(chunk.ClusterId, chunk.NodeId))
				return err
			}
			if chunk.ChunkCount == pb.LastChunkCount {
				plog.Infof("node %d just sent all chunks to %s",
					chunk.From, dn(chunk.ClusterId, chunk.NodeId))
				return nil
			}
		}
	}
}

func (j *job) processSavedSnapshot() error {
	chunks := make([]pb.Chunk, 0)
	for {
		select {
		case <-j.stopc:
			return ErrStopped
		case chunk := <-j.ch:
			if len(chunks) == 0 && chunk.ChunkId != 0 {
				panic("chunk alignment error")
			}
			chunks = append(chunks, chunk)
			if chunk.ChunkId+1 == chunk.ChunkCount {
				return j.sendSavedChunks(chunks)
			}
		}
	}
}

func (j *job) sendSavedChunks(chunks []pb.Chunk) error {
	for _, chunk := range chunks {
		chunkData := make([]byte, snapshotChunkSize)
		chunk.DeploymentId = j.deploymentID
		if !chunk.Witness {
			data, err := loadChunkData(chunk, chunkData, j.fs)
			if err != nil {
				plog.Errorf("failed to read the snapshot chunk, %v", err)
				return err
			}
			chunk.Data = data
		}
		if err := j.sendChunk(chunk, j.conn); err != nil {
			plog.Debugf("send chunk to %s failed", dn(chunk.ClusterId, chunk.NodeId))
			return err
		}
		if v := j.streamChunkSent.Load(); v != nil {
			v.(func(pb.Chunk))(chunk)
		}
	}
	return nil
}

func (j *job) sendChunk(c pb.Chunk,
	conn raftio.ISnapshotConnection) error {
	if v := j.preStreamChunkSend.Load(); v != nil {
		plog.Infof("pre stream chunk send set")
		updated, shouldSend := v.(StreamChunkSendFunc)(c)
		plog.Infof("shoudSend: %t", shouldSend)
		if !shouldSend {
			plog.Infof("not sending the chunk!")
			return errChunkSendSkipped
		}
		return conn.SendChunk(updated)
	}
	return conn.SendChunk(c)
}
