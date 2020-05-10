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

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/lni/goutils/syncutil"
	gvfs "github.com/lni/goutils/vfs"
	"github.com/mkawserm/dragonboat/v3"
	"github.com/mkawserm/dragonboat/v3/config"
	"github.com/mkawserm/dragonboat/v3/internal/logdb"
	"github.com/mkawserm/dragonboat/v3/internal/logdb/kv/badger"
	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/logger"
	"github.com/mkawserm/dragonboat/v3/raftio"
	sm "github.com/mkawserm/dragonboat/v3/statemachine"
)

const (
	dataDirectoryName = "checkdisk-data-safe-to-delete"
)

var batched = flag.Bool("batched-logdb", false, "use batched logdb")
var cpupprof = flag.Bool("cpu-profiling", false, "run CPU profiling")
var mempprof = flag.Bool("mem-profiling", false, "run mem profiling")
var inmemfs = flag.Bool("inmem-fs", false, "use in-memory filesystem")

func newBatchedLogDB(cfg config.LogDBConfig,
	dirs []string, lldirs []string) (raftio.ILogDB, error) {
	fs := vfs.DefaultFS
	return logdb.NewLogDB(cfg, dirs, lldirs, true, false, fs, badger.NewKVStore)
}

type dummyStateMachine struct{}

func newDummyStateMachine(clusterID uint64, nodeID uint64) sm.IStateMachine {
	return &dummyStateMachine{}
}

func (s *dummyStateMachine) Lookup(query interface{}) (interface{}, error) {
	return query, nil
}

func (s *dummyStateMachine) Update(data []byte) (sm.Result, error) {
	return sm.Result{Value: 1}, nil
}

func (s *dummyStateMachine) SaveSnapshot(w io.Writer,
	fc sm.ISnapshotFileCollection, done <-chan struct{}) error {
	panic("not implemented")
}

func (s *dummyStateMachine) RecoverFromSnapshot(r io.Reader,
	files []sm.SnapshotFile,
	done <-chan struct{}) error {
	panic("not implemented")
}

func (s *dummyStateMachine) Close() error { return nil }

func main() {
	flag.Parse()
	fs := gvfs.Default
	if *inmemfs {
		log.Println("using in-memory fs")
		fs = gvfs.NewMem()
	}
	if *cpupprof {
		f, err := os.Create("cpu.pprof")
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
		log.Println("cpu profile will be saved into file cpu.pprof")
	}
	if *mempprof {
		defer func() {
			f, err := os.Create("mem.pprof")
			if err != nil {
				log.Fatal("could not create memory profile: ", err)
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatal("could not write memory profile: ", err)
			}
		}()
		log.Println("memory profile will be saved into file mem.pprof")
	}
	fs.RemoveAll(dataDirectoryName)
	defer fs.RemoveAll(dataDirectoryName)
	logger.GetLogger("raft").SetLevel(logger.WARNING)
	logger.GetLogger("rsm").SetLevel(logger.WARNING)
	logger.GetLogger("logdb").SetLevel(logger.WARNING)
	logger.GetLogger("transport").SetLevel(logger.WARNING)
	logger.GetLogger("dragonboat").SetLevel(logger.WARNING)
	nhc := config.NodeHostConfig{
		NodeHostDir:    dataDirectoryName,
		RTTMillisecond: 200,
		RaftAddress:    "localhost:26000",
		FS:             fs,
	}
	if *batched {
		log.Println("using batched logdb")
		nhc.LogDBFactory = newBatchedLogDB
	}
	nh, err := dragonboat.NewNodeHost(nhc)
	if err != nil {
		panic(err)
	}
	defer nh.Stop()
	rc := config.Config{
		ClusterID:       1,
		NodeID:          1,
		ElectionRTT:     10,
		HeartbeatRTT:    1,
		CheckQuorum:     true,
		SnapshotEntries: 0,
	}
	nodes := make(map[uint64]string)
	nodes[1] = nhc.RaftAddress
	// use 48 clusters, each with only 1 node
	for i := uint64(1); i <= uint64(48); i++ {
		rc.ClusterID = i
		if err := nh.StartCluster(nodes, false, newDummyStateMachine, rc); err != nil {
			panic(err)
		}
	}
	for i := uint64(1); i <= uint64(48); i++ {
		for j := 0; j < 10000; j++ {
			leaderID, ok, err := nh.GetLeaderID(i)
			if err != nil {
				panic(err)
			}
			if ok && leaderID == 1 {
				break
			}
			time.Sleep(time.Millisecond)
			if j == 9999 {
				panic("failed to elect leader")
			}
		}
	}
	fmt.Printf("all clusters are ready, will keep making proposals for 60 seconds\n")
	doneCh := make(chan struct{}, 1)
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	go func() {
		<-timer.C
		close(doneCh)
	}()
	// keep proposing for 60 seconds
	stopper := syncutil.NewStopper()
	results := make([]uint64, 10000)
	for i := uint64(0); i < 10000; i++ {
		stopper.RunPWorker(func(arg interface{}) {
			workerID := arg.(uint64)
			clusterID := (workerID % 48) + 1
			cs := nh.GetNoOPSession(clusterID)
			cmd := make([]byte, 16)
			results[workerID] = 0
			for {
				for j := 0; j < 32; j++ {
					rs, err := nh.Propose(cs, cmd, 4*time.Second)
					if err != nil {
						panic(err)
					}
					v := <-rs.ResultC()
					if v.Completed() {
						results[workerID] = results[workerID] + 1
						rs.Release()
					}
				}
				select {
				case <-doneCh:
					return
				default:
				}
			}
		}, i)
	}
	stopper.Stop()
	total := uint64(0)
	for _, v := range results {
		total = total + v
	}
	fmt.Printf("total %d, %d proposals per second\n", total, total/60)
}
