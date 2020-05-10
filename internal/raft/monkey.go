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

// +build dragonboat_monkeytest

package raft

import (
	"github.com/mkawserm/dragonboat/v3/internal/server"
)

func (rc *Peer) GetInMemLogSize() uint64 {
	ents := rc.raft.log.inmem.entries
	if len(ents) > 0 {
		if ents[0].Index == rc.raft.applied {
			ents = ents[1:]
		}
	}
	return getEntrySliceInMemSize(ents)
}

func (rc *Peer) GetRateLimiter() *server.RateLimiter {
	return rc.raft.log.inmem.rl
}
