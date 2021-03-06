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

package transport

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lni/goutils/leaktest"
	"github.com/lni/goutils/netutil"
	"github.com/lni/goutils/syncutil"
	"github.com/mkawserm/dragonboat/v3/config"
	"github.com/mkawserm/dragonboat/v3/internal/rsm"
	"github.com/mkawserm/dragonboat/v3/internal/server"
	"github.com/mkawserm/dragonboat/v3/internal/settings"
	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/raftio"
	"github.com/mkawserm/dragonboat/v3/raftpb"
)

const (
	serverAddress     = "localhost:5601"
	snapshotDir       = "gtransport_test_data_safe_to_delete"
	caFile            = "tests/test-root-ca.crt"
	certFile          = "tests/localhost.crt"
	keyFile           = "tests/localhost.key"
	testSnapshotIndex = uint64(12345)
)

type dummyTransportEvent struct{}

func (d *dummyTransportEvent) ConnectionEstablished(addr string, snapshot bool) {}
func (d *dummyTransportEvent) ConnectionFailed(addr string, snapshot bool)      {}

type testSnapshotDir struct {
	fs vfs.IFS
}

func newTestSnapshotDir(fs vfs.IFS) *testSnapshotDir {
	return &testSnapshotDir{fs: fs}
}

func (g *testSnapshotDir) GetSnapshotRootDir(clusterID uint64,
	nodeID uint64) string {
	snapNodeDir := fmt.Sprintf("snapshot-%d-%d", clusterID, nodeID)
	return g.fs.PathJoin(snapshotDir, snapNodeDir)
}

func (g *testSnapshotDir) GetSnapshotDir(clusterID uint64,
	nodeID uint64, lastApplied uint64) string {
	snapNodeDir := fmt.Sprintf("snapshot-%d-%d", clusterID, nodeID)
	snapDir := fmt.Sprintf("snapshot-%016X", lastApplied)
	d := g.fs.PathJoin(snapshotDir, snapNodeDir, snapDir)
	return d
}

func (g *testSnapshotDir) getSnapshotFileMD5(clusterID uint64,
	nodeID uint64, index uint64, filename string) ([]byte, error) {
	snapDir := g.GetSnapshotDir(clusterID, nodeID, index)
	fp := g.fs.PathJoin(snapDir, filename)
	f, err := g.fs.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (g *testSnapshotDir) generateSnapshotExternalFile(clusterID uint64,
	nodeID uint64, index uint64, filename string, sz uint64) {
	snapDir := g.GetSnapshotDir(clusterID, nodeID, index)
	if err := g.fs.MkdirAll(snapDir, 0755); err != nil {
		panic(err)
	}
	fp := g.fs.PathJoin(snapDir, filename)
	data := make([]byte, sz)
	rand.Read(data)
	f, err := g.fs.Create(fp)
	if err != nil {
		panic(err)
	}
	n, err := f.Write(data)
	if n != len(data) {
		panic("failed to write all files")
	}
	if err != nil {
		panic(err)
	}
	f.Close()
}

func (g *testSnapshotDir) generateSnapshotFile(clusterID uint64,
	nodeID uint64, index uint64, filename string, sz uint64, fs vfs.IFS) {
	snapDir := g.GetSnapshotDir(clusterID, nodeID, index)
	if err := g.fs.MkdirAll(snapDir, 0755); err != nil {
		panic(err)
	}
	fp := g.fs.PathJoin(snapDir, filename)
	data := make([]byte, sz)
	rand.Read(data)
	writer, err := rsm.NewSnapshotWriter(fp,
		rsm.SnapshotVersion, raftpb.NoCompression, fs)
	if err != nil {
		panic(err)
	}
	n, err := writer.Write(data)
	if n != len(data) {
		panic("short write")
	}
	if err != nil {
		panic(err)
	}
	n, err = writer.Write(data)
	if n != len(data) {
		panic("short write")
	}
	if err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
}

func (g *testSnapshotDir) cleanup() {
	if err := g.fs.RemoveAll(snapshotDir); err != nil {
		panic(err)
	}
}

type testMessageHandler struct {
	mu                        sync.Mutex
	requestCount              map[raftio.NodeInfo]uint64
	unreachableCount          map[raftio.NodeInfo]uint64
	snapshotCount             map[raftio.NodeInfo]uint64
	snapshotFailedCount       map[raftio.NodeInfo]uint64
	snapshotSuccessCount      map[raftio.NodeInfo]uint64
	receivedSnapshotCount     map[raftio.NodeInfo]uint64
	receivedSnapshotFromCount map[raftio.NodeInfo]uint64
}

func newTestMessageHandler() *testMessageHandler {
	return &testMessageHandler{
		requestCount:              make(map[raftio.NodeInfo]uint64),
		unreachableCount:          make(map[raftio.NodeInfo]uint64),
		snapshotCount:             make(map[raftio.NodeInfo]uint64),
		snapshotFailedCount:       make(map[raftio.NodeInfo]uint64),
		snapshotSuccessCount:      make(map[raftio.NodeInfo]uint64),
		receivedSnapshotCount:     make(map[raftio.NodeInfo]uint64),
		receivedSnapshotFromCount: make(map[raftio.NodeInfo]uint64),
	}
}

func (h *testMessageHandler) HandleMessageBatch(reqs raftpb.MessageBatch) (uint64, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ss := uint64(0)
	msg := uint64(0)
	for _, req := range reqs.Requests {
		epk := raftio.GetNodeInfo(req.ClusterId, req.To)
		v, ok := h.requestCount[epk]
		if ok {
			h.requestCount[epk] = v + 1
		} else {
			h.requestCount[epk] = 1
		}
		if req.Type == raftpb.InstallSnapshot {
			ss++
			v, ok = h.snapshotCount[epk]
			if ok {
				h.snapshotCount[epk] = v + 1
			} else {
				h.snapshotCount[epk] = 1
			}
		} else {
			msg++
		}
	}
	return ss, msg
}

func (h *testMessageHandler) HandleSnapshotStatus(clusterID uint64,
	nodeID uint64, failed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	epk := raftio.GetNodeInfo(clusterID, nodeID)
	var p *map[raftio.NodeInfo]uint64
	if failed {
		p = &h.snapshotFailedCount
	} else {
		p = &h.snapshotSuccessCount
	}
	v, ok := (*p)[epk]
	if ok {
		(*p)[epk] = v + 1
	} else {
		(*p)[epk] = 1
	}
}

func (h *testMessageHandler) HandleUnreachable(clusterID uint64,
	nodeID uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	epk := raftio.GetNodeInfo(clusterID, nodeID)
	v, ok := h.unreachableCount[epk]
	if ok {
		h.unreachableCount[epk] = v + 1
	} else {
		h.unreachableCount[epk] = 1
	}
}

func (h *testMessageHandler) HandleSnapshot(clusterID uint64,
	nodeID uint64, from uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	epk := raftio.GetNodeInfo(clusterID, nodeID)
	v, ok := h.receivedSnapshotCount[epk]
	if ok {
		h.receivedSnapshotCount[epk] = v + 1
	} else {
		h.receivedSnapshotCount[epk] = 1
	}
	epk.NodeID = from
	v, ok = h.receivedSnapshotFromCount[epk]
	if ok {
		h.receivedSnapshotFromCount[epk] = v + 1
	} else {
		h.receivedSnapshotFromCount[epk] = 1
	}
}

func (h *testMessageHandler) getReceivedSnapshotCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.receivedSnapshotCount, clusterID, nodeID)
}

func (h *testMessageHandler) getReceivedSnapshotFromCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.receivedSnapshotFromCount, clusterID, nodeID)
}

func (h *testMessageHandler) getRequestCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.requestCount, clusterID, nodeID)
}

func (h *testMessageHandler) getFailedSnapshotCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.snapshotFailedCount, clusterID, nodeID)
}

func (h *testMessageHandler) getSnapshotSuccessCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.snapshotSuccessCount, clusterID, nodeID)
}

func (h *testMessageHandler) getSnapshotCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.snapshotCount, clusterID, nodeID)
}

func (h *testMessageHandler) getUnreachableCount(clusterID uint64,
	nodeID uint64) uint64 {
	return h.getMessageCount(h.unreachableCount, clusterID, nodeID)
}

func (h *testMessageHandler) getMessageCount(m map[raftio.NodeInfo]uint64,
	clusterID uint64, nodeID uint64) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	epk := raftio.GetNodeInfo(clusterID, nodeID)
	v, ok := m[epk]
	if ok {
		return v
	}
	return 0
}

func newNOOPTestTransport(fs vfs.IFS) (*Transport,
	*Nodes, *NOOPTransport, *noopRequest, *noopConnectRequest) {
	t := newTestSnapshotDir(fs)
	nodes := NewNodes(settings.Soft.StreamConnections)
	c := config.NodeHostConfig{
		MaxSendQueueSize: 256 * 1024 * 1024,
		RaftAddress:      "localhost:9876",
		RaftRPCFactory:   NewNOOPTransport,
	}
	ctx, err := server.NewContext(c, fs)
	if err != nil {
		panic(err)
	}
	transport, err := NewTransport(c, ctx, nodes, t.GetSnapshotRootDir, &dummyTransportEvent{}, fs)
	if err != nil {
		panic(err)
	}
	raftRPC, ok := transport.raftRPC.(*NOOPTransport)
	if !ok {
		panic("not a noop transport")
	}
	return transport, nodes, raftRPC, raftRPC.req, raftRPC.connReq
}

func newTestTransport(mutualTLS bool, fs vfs.IFS) (*Transport, *Nodes,
	*syncutil.Stopper, *testSnapshotDir) {
	stopper := syncutil.NewStopper()
	nodes := NewNodes(settings.Soft.StreamConnections)
	t := newTestSnapshotDir(fs)
	c := config.NodeHostConfig{
		RaftAddress: serverAddress,
	}
	if mutualTLS {
		c.MutualTLS = true
		c.CAFile = caFile
		c.CertFile = certFile
		c.KeyFile = keyFile
	}
	ctx, err := server.NewContext(c, fs)
	if err != nil {
		panic(err)
	}
	transport, err := NewTransport(c, ctx, nodes, t.GetSnapshotRootDir, &dummyTransportEvent{}, fs)
	if err != nil {
		panic(err)
	}
	return transport, nodes, stopper, t
}

func testMessageCanBeSent(t *testing.T, mutualTLS bool, sz uint64, fs vfs.IFS) {
	trans, nodes, stopper, _ := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	for i := 0; i < 20; i++ {
		msg := raftpb.Message{
			Type:      raftpb.Heartbeat,
			To:        2,
			ClusterId: 100,
		}
		done := trans.ASyncSend(msg)
		if !done {
			t.Errorf("failed to send message")
		}
	}
	done := false
	for i := 0; i < 200; i++ {
		time.Sleep(100 * time.Millisecond)
		count := handler.getRequestCount(100, 2)
		if count == 20 {
			done = true
			break
		} else {
			plog.Infof("count: %d, want 20, will wait for 100ms", count)
		}
	}
	if !done {
		t.Errorf("failed to get all 20 sent messages")
	}
	// test to ensure a single big message can be sent/received.
	payload := make([]byte, sz)
	m := raftpb.Message{
		Type:      raftpb.Replicate,
		To:        2,
		ClusterId: 100,
		Entries: []raftpb.Entry{
			{
				Cmd: payload,
			},
		},
	}
	ok := trans.ASyncSend(m)
	if !ok {
		t.Errorf("failed to send the large msg")
	}
	received := false
	for i := 0; i < 200; i++ {
		time.Sleep(100 * time.Millisecond)
		if handler.getRequestCount(100, 2) == 21 {
			received = true
			break
		}
	}
	if !received {
		t.Errorf("got %d, want %d", handler.getRequestCount(100, 2), 21)
	}
}

func TestMessageCanBeSent(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testMessageCanBeSent(t, false, settings.LargeEntitySize*2, fs)
	testMessageCanBeSent(t, false, recvBufSize/2, fs)
	testMessageCanBeSent(t, false, recvBufSize+1, fs)
	testMessageCanBeSent(t, false, perConnBufSize+1, fs)
	testMessageCanBeSent(t, false, perConnBufSize/2, fs)
	testMessageCanBeSent(t, false, 1, fs)
	testMessageCanBeSent(t, true, settings.LargeEntitySize*2, fs)
	testMessageCanBeSent(t, true, recvBufSize/2, fs)
	testMessageCanBeSent(t, true, recvBufSize+1, fs)
	testMessageCanBeSent(t, true, perConnBufSize+1, fs)
	testMessageCanBeSent(t, true, perConnBufSize/2, fs)
	testMessageCanBeSent(t, true, 1, fs)
}

// add some latency to localhost
// sudo tc qdisc add dev lo root handle 1:0 netem delay 100msec
// remove latency
// sudo tc qdisc del dev lo root
// don't forget to change your TCP window size if necessary
// e.g. in our dev environment, we have -
// net.core.wmem_max = 25165824
// net.core.rmem_max = 25165824
// net.ipv4.tcp_rmem = 4096 87380 25165824
// net.ipv4.tcp_wmem = 4096 87380 25165824
func testMessageCanBeSentWithLargeLatency(t *testing.T, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, _ := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	for i := 0; i < 128; i++ {
		msg := raftpb.Message{
			Type:      raftpb.Replicate,
			To:        2,
			ClusterId: 100,
			Entries:   []raftpb.Entry{{Cmd: make([]byte, 2*1024*1024)}},
		}
		done := trans.ASyncSend(msg)
		if !done {
			t.Errorf("failed to send message")
		}
	}
	done := false
	for i := 0; i < 400; i++ {
		time.Sleep(100 * time.Millisecond)
		if handler.getRequestCount(100, 2) == 128 {
			done = true
			break
		}
	}
	if !done {
		t.Errorf("failed to send/receive all messages")
	}
}

// latency need to be simulated by configuring your environment
func TestMessageCanBeSentWithLargeLatency(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testMessageCanBeSentWithLargeLatency(t, true, fs)
	testMessageCanBeSentWithLargeLatency(t, false, fs)
}

func testNothingSentBeforeDeploymentIDIsSet(t *testing.T, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, _ := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	for i := 0; i < 100; i++ {
		msg := raftpb.Message{
			Type:      raftpb.Heartbeat,
			To:        2,
			ClusterId: 100,
		}
		// silently drop
		done := trans.ASyncSend(msg)
		if !done {
			t.Errorf("failed to send message")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if handler.getRequestCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getRequestCount(100, 2), 0)
	}
}

func TestNothingSentBeforeDeploymentIDIsSet(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testNothingSentBeforeDeploymentIDIsSet(t, true, fs)
	testNothingSentBeforeDeploymentIDIsSet(t, false, fs)
}

func testMessageBatchWithNotMatchedDBVAreDropped(t *testing.T,
	f SendMessageBatchFunc, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, _ := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	trans.SetPreSendMessageBatchHook(f)
	for i := 0; i < 100; i++ {
		msg := raftpb.Message{
			Type:      raftpb.Heartbeat,
			To:        2,
			ClusterId: 100,
		}
		// silently drop
		done := trans.ASyncSend(msg)
		if !done {
			t.Errorf("failed to send message")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if handler.getRequestCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getRequestCount(100, 2), 0)
	}
}

func TestMessageBatchWithNotMatchedDeploymentIDAreDropped(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	f := func(b raftpb.MessageBatch) (raftpb.MessageBatch, bool) {
		b.DeploymentId = 1
		return b, true
	}
	testMessageBatchWithNotMatchedDBVAreDropped(t, f, true, fs)
	testMessageBatchWithNotMatchedDBVAreDropped(t, f, false, fs)
}

func TestMessageBatchWithNotMatchedBinVerAreDropped(t *testing.T) {
	defer leaktest.AfterTest(t)()
	f := func(b raftpb.MessageBatch) (raftpb.MessageBatch, bool) {
		b.BinVer = raftio.RPCBinVersion + 1
		return b, true
	}
	fs := vfs.GetTestFS()
	testMessageBatchWithNotMatchedDBVAreDropped(t, f, true, fs)
	testMessageBatchWithNotMatchedDBVAreDropped(t, f, false, fs)
}

func TestCircuitBreaker(t *testing.T) {
	defer leaktest.AfterTest(t)()
	breaker := netutil.NewBreaker()
	breaker.Fail()
	if breaker.Ready() {
		t.Errorf("breaker is still ready?")
	}
}

func (t *Transport) queueSize() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.mu.queues)
}

func TestCircuitBreakerKicksInOnConnectivityIssue(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	trans, nodes, stopper, _ := newTestTransport(false, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	nodes.AddNode(100, 2, "nosuchhost:39001")
	msg := raftpb.Message{
		Type:      raftpb.Heartbeat,
		To:        2,
		ClusterId: 100,
	}
	done := trans.ASyncSend(msg)
	if !done {
		t.Errorf("not suppose to fail")
	}
	for {
		if trans.queueSize() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	breaker := trans.GetCircuitBreaker("nosuchhost:39001")
	if breaker.Ready() {
		t.Errorf("breaker is still ready?")
	}
	time.Sleep(time.Second)
	if !breaker.Ready() {
		t.Errorf("breaker is not ready after wait")
	}
	if handler.getUnreachableCount(100, 2) == 0 {
		t.Errorf("unreachable count %d, want 1",
			handler.getUnreachableCount(100, 2))
	}
}

func getTestSnapshotMessage(to uint64) raftpb.Message {
	m := raftpb.Message{
		Type:      raftpb.InstallSnapshot,
		From:      12,
		To:        to,
		ClusterId: 100,
		Snapshot: raftpb.Snapshot{
			Membership: raftpb.Membership{
				ConfigChangeId: 178,
			},
			Index: testSnapshotIndex,
			Term:  19,
		},
	}

	return m
}

func TestSnapshotCanBeSent(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	mutualTLSValues := []bool{true, false}
	for _, v := range mutualTLSValues {
		testSnapshotCanBeSent(t, snapshotChunkSize-1, 3000, v, fs)
		testSnapshotCanBeSent(t, snapshotChunkSize/2, 3000, v, fs)
		testSnapshotCanBeSent(t, snapshotChunkSize+1, 3000, v, fs)
		testSnapshotCanBeSent(t, snapshotChunkSize*3, 3000, v, fs)
		testSnapshotCanBeSent(t, snapshotChunkSize*3+1, 3000, v, fs)
		testSnapshotCanBeSent(t, snapshotChunkSize*3-1, 3000, v, fs)
	}
}

func TestLargeSnapshotCanBeSent(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testSnapshotCanBeSent(t, 64*1024*1024, 20000, true, fs)
	testSnapshotCanBeSent(t, 256*1024*1024+5, 20000, false, fs)
}

func testSourceAddressWillBeAddedToNodeRegistry(t *testing.T, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, _ := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	msg := raftpb.Message{
		Type:      raftpb.Heartbeat,
		To:        2,
		From:      200,
		ClusterId: 100,
	}
	done := trans.ASyncSend(msg)
	if !done {
		t.Errorf("not suppose to fail")
	}
	count := 0
	for count < 200 && handler.getRequestCount(100, 2) == 0 {
		count++
		time.Sleep(5 * time.Millisecond)
	}
	if count == 200 {
		t.Errorf("failed to send the message")
	}
	if len(nodes.nmu.nodes) != 1 {
		t.Errorf("remote address not updated")
	}
	key := raftio.GetNodeInfo(100, 200)
	v, ok := nodes.nmu.nodes[key]
	if !ok {
		t.Errorf("did not record source address")
	}
	if v != serverAddress {
		t.Errorf("v %s, want %s", v, serverAddress)
	}
}

func TestSourceAddressWillBeAddedToNodeRegistry(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testSourceAddressWillBeAddedToNodeRegistry(t, true, fs)
	testSourceAddressWillBeAddedToNodeRegistry(t, false, fs)
}

func waitForTotalSnapshotStatusUpdateCount(handler *testMessageHandler,
	maxWait uint64, count uint64) {
	total := uint64(0)

	for total < maxWait {
		time.Sleep(10 * time.Millisecond)
		total += 10
		handler.mu.Lock()
		c := uint64(0)
		for _, v := range handler.snapshotFailedCount {
			c += v
		}
		for _, v := range handler.snapshotSuccessCount {
			c += v
		}
		handler.mu.Unlock()

		if c >= count {
			return
		}
	}

}

func waitForFirstSnapshotStatusUpdate(handler *testMessageHandler,
	maxWait uint64) {
	total := uint64(0)
	for total < maxWait {
		time.Sleep(10 * time.Millisecond)
		total += 10
		if handler.getFailedSnapshotCount(100, 2) > 0 ||
			handler.getSnapshotSuccessCount(100, 2) > 0 {
			return
		}
	}
}

func waitForSnapshotCountUpdate(handler *testMessageHandler, maxWait uint64) {
	total := uint64(0)
	for total < maxWait {
		time.Sleep(10 * time.Millisecond)
		total += 10
		count := handler.getReceivedSnapshotCount(100, 2)
		if count > 0 {
			return
		}
	}
}

func getTestSnapshotFileSize(sz uint64) uint64 {
	if rsm.SnapshotVersion == rsm.V1SnapshotVersion {
		return sz*2 + rsm.SnapshotHeaderSize
	} else if rsm.SnapshotVersion == rsm.V2SnapshotVersion {
		return rsm.GetV2PayloadSize(sz*2) + rsm.SnapshotHeaderSize
	} else {
		panic("unknown snapshot version")
	}
}

func testSnapshotCanBeSent(t *testing.T,
	sz uint64, maxWait uint64, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer func() {
		if err := fs.RemoveAll(snapshotDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot.gbsnap", sz, fs)
	m := getTestSnapshotMessage(2)
	m.Snapshot.FileSize = getTestSnapshotFileSize(sz)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	chunks := NewChunks(trans.handleRequest,
		trans.snapshotReceived, getTestDeploymentID, trans.folder, fs)
	snapDir := chunks.folder(100, 2)
	if err := fs.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("%v", err)
	}
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	done := trans.ASyncSendSnapshot(m)
	if !done {
		t.Errorf("failed to send the snapshot")
	}
	waitForFirstSnapshotStatusUpdate(handler, maxWait)
	waitForSnapshotCountUpdate(handler, maxWait)
	if handler.getSnapshotCount(100, 2) != 1 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 1)
	}
	if handler.getFailedSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want 0", handler.getFailedSnapshotCount(100, 2))
	}
	if handler.getSnapshotSuccessCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getSnapshotSuccessCount(100, 2))
	}
	if handler.getReceivedSnapshotFromCount(100, 12) != 1 {
		t.Errorf("got %d, want 1", handler.getReceivedSnapshotFromCount(100, 12))
	}
	if handler.getReceivedSnapshotCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getReceivedSnapshotFromCount(100, 12))
	}
	md5Original, err := tt.getSnapshotFileMD5(100,
		2, testSnapshotIndex, "testsnapshot.gbsnap")
	if err != nil {
		t.Errorf("err %v, want nil", err)
	}
	md5Received, err := tt.getSnapshotFileMD5(100,
		12, testSnapshotIndex, "testsnapshot.gbsnap")
	if err != nil {
		t.Errorf("err %v, want nil", err)
	}
	if !bytes.Equal(md5Original, md5Received) {
		t.Errorf("snapshot content changed during transmission")
	}
}

func testSnapshotWithNotMatchedDBVWillBeDropped(t *testing.T,
	f StreamChunkSendFunc, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot.gbsnap", 1024, fs)
	m := getTestSnapshotMessage(2)
	m.Snapshot.FileSize = getTestSnapshotFileSize(1024)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	trans.SetPreStreamChunkSendHook(f)
	done := trans.ASyncSendSnapshot(m)
	if !done {
		t.Errorf("failed to send the snapshot")
	}
	waitForFirstSnapshotStatusUpdate(handler, 1000)
	waitForSnapshotCountUpdate(handler, 1000)
	if handler.getSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 0)
	}
	// such snapshot dropped on the sending side should be reported.
	if handler.getFailedSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want 0", handler.getFailedSnapshotCount(100, 2))
	}
	if handler.getSnapshotSuccessCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getSnapshotSuccessCount(100, 2))
	}
}

func TestSnapshotWithNotMatchedDeploymentIDWillBeDropped(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	f := func(c raftpb.Chunk) (raftpb.Chunk, bool) {
		c.DeploymentId = 1
		return c, true
	}
	testSnapshotWithNotMatchedDBVWillBeDropped(t, f, true, fs)
	testSnapshotWithNotMatchedDBVWillBeDropped(t, f, false, fs)
}

func TestSnapshotWithNotMatchedBinVerWillBeDropped(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	f := func(c raftpb.Chunk) (raftpb.Chunk, bool) {
		c.BinVer = raftio.RPCBinVersion + 1
		return c, true
	}
	testSnapshotWithNotMatchedDBVWillBeDropped(t, f, true, fs)
	testSnapshotWithNotMatchedDBVWillBeDropped(t, f, false, fs)
}

func testFailedSnapshotLoadChunkWillBeReported(t *testing.T,
	mutualTLS bool, fs vfs.IFS) {
	snapshotSize := uint64(snapshotChunkSize) * 10
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer func() {
		if err := fs.RemoveAll(snapshotDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	chunks := NewChunks(trans.handleRequest,
		trans.snapshotReceived, getTestDeploymentID, trans.folder, fs)
	snapDir := chunks.folder(100, 2)
	if err := fs.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("%v", err)
	}
	nodes.AddNode(100, 2, serverAddress)
	onStreamChunkSent := func(c raftpb.Chunk) {
		snapDir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
		fp := fs.PathJoin(snapDir, "testsnapshot.gbsnap")
		err := fs.Remove(fp)
		if err != nil {
			plog.Errorf("failed to remove file %v", err)
		} else {
			plog.Infof("test file removed: %s", fp)
		}
	}
	trans.streamChunkSent.Store(onStreamChunkSent)
	tt.generateSnapshotFile(100,
		12, testSnapshotIndex, "testsnapshot.gbsnap", snapshotSize, fs)
	m := getTestSnapshotMessage(2)
	m.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	done := trans.ASyncSendSnapshot(m)
	if !done {
		t.Errorf("failed to send the snapshot")
	}
	waitForFirstSnapshotStatusUpdate(handler, 5000)
	if handler.getSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 0)
	}
	if handler.getFailedSnapshotCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getFailedSnapshotCount(100, 2))
	}
	if handler.getSnapshotSuccessCount(100, 2) != 0 {
		t.Errorf("got %d, want 0", handler.getSnapshotSuccessCount(100, 2))
	}
}

func TestMaxSnapshotConnectionIsLimited(t *testing.T) {
	fs := vfs.GetTestFS()
	trans, nodes, stopper, tt := newTestTransport(false, fs)
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	conns := make([]*Sink, 0)
	for i := uint32(0); i < maxConnectionCount; i++ {
		sink := trans.GetStreamConnection(100, 2)
		if sink == nil {
			t.Errorf("failed to get sink")
		}
		conns = append(conns, sink)
	}
	for i := uint32(0); i < maxConnectionCount; i++ {
		sink := trans.GetStreamConnection(100, 2)
		if sink != nil {
			t.Errorf("connection is not limited")
		}
	}
	for _, v := range conns {
		close(v.j.ch)
	}
	for {
		if atomic.LoadUint32(&trans.jobs) != 0 {
			time.Sleep(time.Millisecond)
		} else {
			break
		}
	}
	breaker := trans.GetCircuitBreaker(serverAddress)
	for {
		breaker.Success()
		if breaker.Ready() {
			break
		}
	}
	for i := uint32(0); i < maxConnectionCount; i++ {
		sink := trans.GetStreamConnection(100, 2)
		if sink == nil {
			t.Errorf("failed to get sink again %d", i)
		}
	}
}

func TestFailedSnapshotLoadChunkWillBeReported(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testFailedSnapshotLoadChunkWillBeReported(t, false, fs)
	testFailedSnapshotLoadChunkWillBeReported(t, true, fs)
}

func testFailedConnectionReportsSnapshotFailure(t *testing.T,
	mutualTLS bool, fs vfs.IFS) {
	snapshotSize := uint64(snapshotChunkSize) * 10
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	// invalid address
	nodes.AddNode(100, 2, "localhost:12345")
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot.gbsnap", snapshotSize, fs)
	m := getTestSnapshotMessage(2)
	m.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	done := trans.ASyncSendSnapshot(m)
	if !done {
		t.Errorf("failed to send the snapshot")
	}
	savedTimeout := getDialTimeoutSecond()
	defer func() {
		setDialTimeoutSecond(savedTimeout)
	}()
	setDialTimeoutSecond(1)
	waitForTotalSnapshotStatusUpdateCount(handler, 6000, 1)
	if handler.getSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 0)
	}
	if handler.getFailedSnapshotCount(100, 2) == 0 {
		t.Errorf("got %d, want > 0", handler.getFailedSnapshotCount(100, 2))
	}
}

func TestFailedConnectionReportsSnapshotFailure(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testFailedConnectionReportsSnapshotFailure(t, true, fs)
	testFailedConnectionReportsSnapshotFailure(t, false, fs)
}

func testFailedSnapshotSendWillBeReported(t *testing.T, mutualTLS bool, fs vfs.IFS) {
	snapshotSize := uint64(snapshotChunkSize) * 10
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	nodes.AddNode(100, 2, serverAddress)
	nodes.AddNode(100, 3, serverAddress)
	snapshotSent := uint32(0)
	f := func(c raftpb.Chunk) (raftpb.Chunk, bool) {
		for atomic.LoadUint32(&snapshotSent) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		return c, false
	}
	trans.SetPreStreamChunkSendHook(f)
	// send two snapshots to the same node {100:2}
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot.gbsnap", snapshotSize, fs)
	m := getTestSnapshotMessage(2)
	m.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	trans.ASyncSendSnapshot(m)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot2.gbsnap", snapshotSize, fs)
	m2 := getTestSnapshotMessage(2)
	m2.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	m2.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot1.gbsnap")
	// send the snapshot file
	trans.ASyncSendSnapshot(m2)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot3.gbsnap", snapshotSize, fs)
	m3 := getTestSnapshotMessage(3)
	m3.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	m3.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	// send the snapshot file
	trans.ASyncSendSnapshot(m3)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot4.gbsnap", snapshotSize, fs)
	m4 := getTestSnapshotMessage(2)
	m4.Snapshot.FileSize = getTestSnapshotFileSize(snapshotSize)
	m4.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot2.gbsnap")
	// send the snapshot file
	trans.ASyncSendSnapshot(m4)
	atomic.StoreUint32(&snapshotSent, 1)
	waitForTotalSnapshotStatusUpdateCount(handler, 1000, 4)
	if handler.getSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 0)
	}
	if handler.getSnapshotCount(100, 3) != 0 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 3), 0)
	}
	if handler.getFailedSnapshotCount(100, 2) != 3 {
		t.Errorf("got %d, want 3", handler.getFailedSnapshotCount(100, 2))
	}
	if handler.getFailedSnapshotCount(100, 3) != 1 {
		t.Errorf("got %d, want 1", handler.getFailedSnapshotCount(100, 3))
	}
}

func TestFailedSnapshotSendWillBeReported(t *testing.T) {
	fs := vfs.GetTestFS()
	defer leaktest.AfterTest(t)()
	testFailedSnapshotSendWillBeReported(t, true, fs)
	testFailedSnapshotSendWillBeReported(t, false, fs)
}

func testSnapshotWithExternalFilesCanBeSend(t *testing.T,
	sz uint64, maxWait uint64, mutualTLS bool, fs vfs.IFS) {
	trans, nodes, stopper, tt := newTestTransport(mutualTLS, fs)
	defer func() {
		if err := fs.RemoveAll(snapshotDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	defer trans.serverCtx.Stop()
	defer tt.cleanup()
	defer trans.Stop()
	defer stopper.Stop()
	handler := newTestMessageHandler()
	trans.SetMessageHandler(handler)
	trans.SetDeploymentID(12345)
	chunks := NewChunks(trans.handleRequest,
		trans.snapshotReceived, getTestDeploymentID, trans.folder, fs)
	ts := getTestChunks()
	snapDir := chunks.folder(ts[0].ClusterId, ts[0].NodeId)
	if err := fs.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("%v", err)
	}
	nodes.AddNode(100, 2, serverAddress)
	tt.generateSnapshotFile(100, 12, testSnapshotIndex, "testsnapshot.gbsnap", sz, fs)
	tt.generateSnapshotExternalFile(100, 12, testSnapshotIndex, "external1.data", sz)
	tt.generateSnapshotExternalFile(100, 12, testSnapshotIndex, "external2.data", sz)
	m := getTestSnapshotMessage(2)
	dir := tt.GetSnapshotDir(100, 12, testSnapshotIndex)
	m.Snapshot.FileSize = getTestSnapshotFileSize(sz)
	m.Snapshot.Filepath = fs.PathJoin(dir, "testsnapshot.gbsnap")
	f1 := &raftpb.SnapshotFile{
		Filepath: fs.PathJoin(dir, "external1.data"),
		FileSize: sz,
		FileId:   1,
	}
	f2 := &raftpb.SnapshotFile{
		Filepath: fs.PathJoin(dir, "external2.data"),
		FileSize: sz,
		FileId:   2,
	}
	m.Snapshot.Files = []*raftpb.SnapshotFile{f1, f2}
	// send the snapshot file
	done := trans.ASyncSendSnapshot(m)
	if !done {
		t.Errorf("failed to send the snapshot")
	}
	waitForFirstSnapshotStatusUpdate(handler, maxWait)
	waitForSnapshotCountUpdate(handler, maxWait)
	if handler.getSnapshotCount(100, 2) != 1 {
		t.Errorf("got %d, want %d", handler.getSnapshotCount(100, 2), 1)
	}
	if handler.getFailedSnapshotCount(100, 2) != 0 {
		t.Errorf("got %d, want 0", handler.getFailedSnapshotCount(100, 2))
	}
	if handler.getSnapshotSuccessCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getSnapshotSuccessCount(100, 2))
	}
	if handler.getReceivedSnapshotFromCount(100, 12) != 1 {
		t.Errorf("got %d, want 1", handler.getReceivedSnapshotFromCount(100, 12))
	}
	if handler.getReceivedSnapshotCount(100, 2) != 1 {
		t.Errorf("got %d, want 1", handler.getReceivedSnapshotFromCount(100, 12))
	}
	filenames := []string{"testsnapshot.gbsnap", "external1.data", "external2.data"}
	for _, fn := range filenames {
		md5Original, err := tt.getSnapshotFileMD5(100, 2, testSnapshotIndex, fn)
		if err != nil {
			t.Errorf("err %v, want nil", err)
		}
		md5Received, err := tt.getSnapshotFileMD5(100, 12, testSnapshotIndex, fn)
		if err != nil {
			t.Errorf("err %v, want nil", err)
		}
		if !bytes.Equal(md5Original, md5Received) {
			t.Errorf("snapshot content changed during transmission")
		}
	}
}

func TestSnapshotWithExternalFilesCanBeSend(t *testing.T) {
	fs := vfs.GetTestFS()
	testSnapshotWithExternalFilesCanBeSend(t, snapshotChunkSize/2, 3000, false, fs)
	testSnapshotWithExternalFilesCanBeSend(t, snapshotChunkSize*3+100, 3000, false, fs)
	testSnapshotWithExternalFilesCanBeSend(t, snapshotChunkSize/2, 3000, true, fs)
	testSnapshotWithExternalFilesCanBeSend(t, snapshotChunkSize*3+100, 3000, true, fs)
}

func TestNoOPTransportCanBeCreated(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, _, _, _, _ := newNOOPTestTransport(fs)
	defer tt.Stop()
}

func TestInitialMessageCanBeSent(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, noopRPC, req, connReq := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	msg := raftpb.Message{
		Type:      raftpb.Heartbeat,
		To:        2,
		ClusterId: 100,
	}
	connReq.SetToFail(false)
	req.SetToFail(false)
	ok := tt.ASyncSend(msg)
	if !ok {
		t.Errorf("send failed")
	}
	for i := 0; i < 1000; i++ {
		if atomic.LoadUint64(&noopRPC.connected) != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if tt.queueSize() != 1 {
		t.Errorf("queue len %d, want 1", tt.queueSize())
	}
	if len(tt.mu.breakers) != 1 {
		t.Errorf("breakers len %d, want 1", tt.queueSize())
	}
	if noopRPC.connected != 1 {
		t.Errorf("connected %d, want 1", noopRPC.connected)
	}
}

func TestFailedConnectionIsRemovedFromTransport(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, req, connReq := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	msg := raftpb.Message{
		Type:      raftpb.Heartbeat,
		To:        2,
		ClusterId: 100,
	}
	connReq.SetToFail(false)
	req.SetToFail(false)
	ok := tt.ASyncSend(msg)
	if !ok {
		t.Errorf("send failed")
	}
	req.SetToFail(true)
	ok = tt.ASyncSend(msg)
	if !ok {
		t.Errorf("send failed")
	}
	for i := 0; i < 1000; i++ {
		if tt.queueSize() != 0 {
			time.Sleep(time.Millisecond)
		} else {
			break
		}
	}
	if tt.queueSize() != 0 {
		t.Errorf("queue len %d, want 0", tt.queueSize())
	}
}

func TestCircuitBreakerCauseFailFast(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, noopRPC, req, connReq := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	msg := raftpb.Message{
		Type:      raftpb.Heartbeat,
		To:        2,
		ClusterId: 100,
	}
	connReq.SetToFail(false)
	req.SetToFail(false)
	ok := tt.ASyncSend(msg)
	if !ok {
		t.Errorf("send failed")
	}
	req.SetToFail(true)
	ok = tt.ASyncSend(msg)
	if !ok {
		t.Errorf("send failed")
	}
	for i := 0; i < 1000; i++ {
		if tt.queueSize() != 0 {
			time.Sleep(time.Millisecond)
		} else {
			break
		}
	}
	req.SetToFail(false)
	for i := 0; i < 20; i++ {
		ok = tt.ASyncSend(msg)
		if ok {
			t.Errorf("send unexpectedly returned ok")
		}
		time.Sleep(time.Millisecond)
	}
	if tt.queueSize() != 0 {
		t.Errorf("queue len %d, want 0", tt.queueSize())
	}
	if noopRPC.connected != 1 {
		t.Errorf("connected %d, want 1", noopRPC.connected)
	}
	if noopRPC.tryConnect != 1 {
		t.Errorf("connected %d, want 1", noopRPC.tryConnect)
	}
}

// unknown target
func TestStreamToUnknownTargetWillHaveSnapshotStatusUpdated(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, _, _ := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	sink := tt.GetStreamConnection(100, 3)
	if sink != nil {
		t.Errorf("unexpectedly returned a sink")
	}
	if handler.getFailedSnapshotCount(100, 3) != 1 {
		t.Errorf("snapshot failed count %d", handler.snapshotFailedCount)
	}
	if handler.getSnapshotSuccessCount(100, 3) != 0 {
		t.Errorf("snapshot succeed count %d", handler.snapshotSuccessCount)
	}
}

// failed to connect
func TestFailedStreamConnectionWillHaveSnapshotStatusUpdated(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, req, connReq := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	connReq.SetToFail(true)
	req.SetToFail(true)
	tt.GetStreamConnection(100, 2)
	failedSnapshotReported := false
	for i := 0; i < 10000; i++ {
		if handler.getFailedSnapshotCount(100, 2) != 1 {
			time.Sleep(time.Millisecond)
			continue
		}
		failedSnapshotReported = true
		break
	}
	if !failedSnapshotReported {
		t.Fatalf("failed snapshot not reported")
	}
}

// failed to connect due to too many connections
func TestFailedStreamingDueToTooManyConnectionsHaveStatusUpdated(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, _, _ := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	for i := uint32(0); i < maxConnectionCount; i++ {
		sink := tt.GetStreamConnection(100, 2)
		if sink == nil {
			t.Errorf("failed to connect")
		}
	}
	for i := uint32(0); i < 2*maxConnectionCount; i++ {
		sink := tt.GetStreamConnection(100, 2)
		if sink != nil {
			t.Errorf("stream connection not limited")
		}
	}
	failedSnapshotReported := false
	for i := 0; i < 10000; i++ {
		count := handler.getFailedSnapshotCount(100, 2)
		plog.Infof("count: %d, want %d", count)
		if count != uint64(2*maxConnectionCount) {
			time.Sleep(time.Millisecond)
			continue
		}
		failedSnapshotReported = true
		break
	}
	if !failedSnapshotReported {
		t.Fatalf("failed snapshot not reported")
	}
}

func TestInMemoryEntrySizeCanBeLimitedWhenSendingMessages(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, req, _ := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	e := raftpb.Entry{Cmd: make([]byte, 1024*1024*10)}
	msg := raftpb.Message{
		ClusterId: 100,
		To:        2,
		Type:      raftpb.Replicate,
		Entries:   []raftpb.Entry{e},
	}
	req.SetBlocked(true)
	rled := func(exp bool) {
		_, key, err := nodes.Resolve(100, 2)
		if err != nil {
			t.Fatalf("failed to resolve the addr")
		}
		sq, ok := tt.mu.queues[key]
		if !ok {
			t.Fatalf("failed to get sq")
		}
		if sq.rateLimited() != exp {
			t.Errorf("rate limited unexpected, exp %t", exp)
		}
	}
	for i := 0; i < 100; i++ {
		sent := tt.ASyncSend(msg)
		if !sent {
			rled(true)
			break
		}
		if i == 99 {
			t.Errorf("no message rejected")
		}
	}
	req.SetBlocked(false)
	for i := 0; i < 1000; i++ {
		sent := tt.ASyncSend(msg)
		if sent {
			rled(false)
			break
		}
		time.Sleep(5 * time.Millisecond)
		if i == 999 {
			t.Errorf("no message buffered")
		}
	}
}

func TestInMemoryEntrySizeCanDropToZero(t *testing.T) {
	fs := vfs.GetTestFS()
	tt, nodes, _, _, _ := newNOOPTestTransport(fs)
	defer tt.Stop()
	tt.SetDeploymentID(12345)
	handler := newTestMessageHandler()
	tt.SetMessageHandler(handler)
	nodes.AddNode(100, 2, serverAddress)
	e := raftpb.Entry{Cmd: make([]byte, 1024*1024*10)}
	msg := raftpb.Message{
		ClusterId: 100,
		To:        2,
		Type:      raftpb.Replicate,
		Entries:   []raftpb.Entry{e},
	}
	_, key, err := nodes.Resolve(100, 2)
	if err != nil {
		t.Fatalf("failed to resolve the addr")
	}
	if !tt.ASyncSend(msg) {
		t.Errorf("first send failed")
	}
	sq, ok := tt.mu.queues[key]
	if !ok {
		t.Fatalf("failed to get sq")
	}
	for len(sq.ch) != 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		sent := tt.ASyncSend(msg)
		if !sent {
			t.Errorf("failed to send2")
		}
	}
	for len(sq.ch) != 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 1000; i++ {
		time.Sleep(10 * time.Millisecond)
		if sq.rl.Get() == 0 {
			return
		}
		if i == 999 {
			t.Errorf("rate limiter failed to report correct size")
		}
	}
}
