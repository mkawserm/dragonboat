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
kvtest is IStateMachine plugin used in various tests.
*/
package main

// // required by the plugin system
import "C"

import (
	"github.com/mkawserm/dragonboat/v3/internal/tests"
	"github.com/mkawserm/dragonboat/v3/statemachine"
)

// DragonboatApplicationName is the name of your plugin.
var DragonboatApplicationName string = "kvtest"

// CreateStateMachine create the state machine IStateMachine object.
func CreateStateMachine(clusterID uint64, nodeID uint64) statemachine.IStateMachine {
	return tests.NewKVTest(clusterID, nodeID)
}
