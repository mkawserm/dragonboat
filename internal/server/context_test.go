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

package server

import (
	"fmt"
	"testing"

	"github.com/mkawserm/dragonboat/v3/config"
	"github.com/mkawserm/dragonboat/v3/internal/fileutil"
	"github.com/mkawserm/dragonboat/v3/internal/settings"
	"github.com/mkawserm/dragonboat/v3/internal/vfs"
	"github.com/mkawserm/dragonboat/v3/raftio"
	"github.com/mkawserm/dragonboat/v3/raftpb"
)

const (
	singleNodeHostTestDir = "test_nodehost_dir_safe_to_delete"
	testLogDBName         = "test-name"
	testBinVer            = raftio.LogDBBinVersion
	testAddress           = "localhost:1111"
	testDeploymentID      = 100
)

func getTestNodeHostConfig() config.NodeHostConfig {
	return config.NodeHostConfig{
		WALDir:         singleNodeHostTestDir,
		NodeHostDir:    singleNodeHostTestDir,
		RTTMillisecond: 50,
		RaftAddress:    testAddress,
	}
}

func TestCheckNodeHostDirWorksWhenEverythingMatches(t *testing.T) {
	fs := vfs.GetTestFS()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	func() {
		c := getTestNodeHostConfig()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic not expected")
			}
		}()
		ctx, err := NewContext(c, fs)
		if err != nil {
			t.Fatalf("failed to new context %v", err)
		}
		if _, _, err := ctx.CreateNodeHostDir(testDeploymentID); err != nil {
			t.Fatalf("%v", err)
		}
		dirs, _ := ctx.getDataDirs()
		testName := "test-name"
		status := raftpb.RaftDataStatus{
			Address:      testAddress,
			BinVer:       raftio.LogDBBinVersion,
			HardHash:     settings.Hard.Hash(),
			LogdbType:    testName,
			Hostname:     ctx.hostname,
			DeploymentId: testDeploymentID,
		}
		err = fileutil.CreateFlagFile(dirs[0], addressFilename, &status, fs)
		if err != nil {
			t.Errorf("failed to create flag file %v", err)
		}
		if err := ctx.CheckNodeHostDir(testDeploymentID,
			testAddress, raftio.LogDBBinVersion, testName); err != nil {
			t.Fatalf("check node host dir failed %v", err)
		}
	}()
	reportLeakedFD(fs, t)
}

func testNodeHostDirectoryDetectsMismatches(t *testing.T,
	addr string, hostname string, binVer uint32, name string,
	hardHashMismatch bool, expErr error, fs vfs.IFS) {
	c := getTestNodeHostConfig()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	ctx, err := NewContext(c, fs)
	if err != nil {
		t.Fatalf("failed to new context %v", err)
	}
	if _, _, err := ctx.CreateNodeHostDir(testDeploymentID); err != nil {
		t.Fatalf("%v", err)
	}
	dirs, _ := ctx.getDataDirs()
	status := raftpb.RaftDataStatus{
		Address:   addr,
		BinVer:    binVer,
		HardHash:  settings.Hard.Hash(),
		LogdbType: name,
		Hostname:  hostname,
	}
	if hardHashMismatch {
		status.HardHash = 1
	}
	err = fileutil.CreateFlagFile(dirs[0], addressFilename, &status, fs)
	if err != nil {
		t.Errorf("failed to create flag file %v", err)
	}
	err = ctx.CheckNodeHostDir(testDeploymentID, testAddress, testBinVer, testLogDBName)
	plog.Infof("err: %v", err)
	if err != expErr {
		t.Errorf("expect err %v, got %v", expErr, err)
	}
	reportLeakedFD(fs, t)
}

func TestCanDetectMismatchedHostname(t *testing.T) {
	fs := vfs.GetTestFS()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "incorrect-hostname", raftio.LogDBBinVersion,
		testLogDBName, false, ErrHostnameChanged, fs)
}

// TODO: re-enable this
/*
func TestCanDetectMismatchedLogDBName(t *testing.T) {
	fs := vfs.GetTestFS()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "", raftio.LogDBBinVersion,
		"incorrect name", false, ErrLogDBType, fs)
}*/

func TestCanDetectMismatchedBinVer(t *testing.T) {
	fs := vfs.GetTestFS()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "", raftio.LogDBBinVersion+1,
		testLogDBName, false, ErrIncompatibleData, fs)
}

func TestCanDetectMismatchedAddress(t *testing.T) {
	fs := vfs.GetTestFS()
	testNodeHostDirectoryDetectsMismatches(t,
		"invalid:12345", "", raftio.LogDBBinVersion,
		testLogDBName, false, ErrNotOwner, fs)
}

func TestCanDetectMismatchedHardHash(t *testing.T) {
	fs := vfs.GetTestFS()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "", raftio.LogDBBinVersion,
		testLogDBName, true, ErrHardSettingsChanged, fs)
}

func TestLockFileCanBeLockedAndUnlocked(t *testing.T) {
	fs := vfs.GetTestFS()
	c := getTestNodeHostConfig()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	ctx, err := NewContext(c, fs)
	if err != nil {
		t.Fatalf("failed to new context %v", err)
	}
	if _, _, err := ctx.CreateNodeHostDir(c.DeploymentID); err != nil {
		t.Fatalf("%v", err)
	}
	if err := ctx.LockNodeHostDir(); err != nil {
		t.Fatalf("failed to lock the directory %v", err)
	}
	ctx.Stop()
	reportLeakedFD(fs, t)
}

func TestRemoveSavedSnapshots(t *testing.T) {
	fs := vfs.GetTestFS()
	if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
		t.Fatalf("%v", err)
	}
	if err := fs.MkdirAll(singleNodeHostTestDir, 0755); err != nil {
		t.Fatalf("%v", err)
	}
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	for i := 0; i < 16; i++ {
		ssdir := fs.PathJoin(singleNodeHostTestDir, fmt.Sprintf("snapshot-%X", i))
		if err := fs.MkdirAll(ssdir, 0755); err != nil {
			t.Fatalf("failed to mkdir %v", err)
		}
	}
	for i := 1; i <= 2; i++ {
		ssdir := fs.PathJoin(singleNodeHostTestDir, fmt.Sprintf("mydata-%X", i))
		if err := fs.MkdirAll(ssdir, 0755); err != nil {
			t.Fatalf("failed to mkdir %v", err)
		}
	}
	if err := removeSavedSnapshots(singleNodeHostTestDir, fs); err != nil {
		t.Fatalf("failed to remove saved snapshots %v", err)
	}
	files, err := fs.List(singleNodeHostTestDir)
	if err != nil {
		t.Fatalf("failed to read dir %v", err)
	}
	for _, fn := range files {
		fi, err := fs.Stat(fs.PathJoin(singleNodeHostTestDir, fn))
		if err != nil {
			t.Fatalf("failed to get stat %v", err)
		}
		if !fi.IsDir() {
			t.Errorf("found unexpected file %v", fi)
		}
		if fi.Name() != "mydata-1" && fi.Name() != "mydata-2" {
			t.Errorf("unexpected dir found %s", fi.Name())
		}
	}
	reportLeakedFD(fs, t)
}
