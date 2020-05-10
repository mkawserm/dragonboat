![dragonboat](./docs/dragonboat.jpg)
# Dragonboat - A Multi-Group Raft library in Go ##
[![license](http://img.shields.io/badge/license-Apache2-blue.svg)](https://github.com/lni/dragonboat/blob/master/LICENSE)



## About ##
Dragonboat is a high performance multi-group [Raft](https://raft.github.io/) [consensus](https://en.wikipedia.org/wiki/Consensus_(computer_science)) library in [Go](https://golang.org/).
This fork of Dragonboat uses [Badger](https://github.com/dgraph-io/badger) as raft LogDb.

Consensus algorithms such as Raft provides fault-tolerance by alllowing a system continue to operate as long as the majority member servers are available. For example, a Raft cluster of 5 servers can make progress even if 2 servers fail. It also appears to clients as a single node with strong data consistency always provided. All running servers can be used to initiate read requests for aggregated read throughput.

Dragonboat handles all technical difficulties associated with Raft to allow users to just focus on their application domains. It is also very easy to use, our step-by-step [examples](https://github.com/lni/dragonboat-example) can help new users to master it in half an hour.


## Features ##
* Easy to use API for building Raft based applications
* Feature complete and scalable multi-group Raft implementation
* Disk based and memory based state machine support
* Fully pipelined and TLS mutual authentication support, ready for high latency open environment
* Custom Raft log storage and Raft RPC support, easy to integrate with latest I/O techs
* Prometheus based health metrics support
* Built-in tool to repair Raft clusters that permanently lost the quorum
* [Extensively tested](/docs/test.md) including using [Jepsen](https://aphyr.com/tags/jepsen)'s [Knossos](https://github.com/jepsen-io/knossos) linearizability checker, some results are [here](https://github.com/lni/knossos-data)

Most features covered in Diego Ongaro's [Raft thesis](https://ramcloud.stanford.edu/~ongaro/thesis.pdf) have been supported -
* leader election, log replication, snapshotting and log compaction
* membership change
* ReadIndex protocol for read-only queries
* leadership transfer
* non-voting member
* witness member
* idempotent update transparent to applications
* batching and pipelining
* disk based state machine

## License ##
Dragonboat is licensed under the Apache License Version 2.0. See LICENSE for details.

Third party code used in Dragonboat and their licenses is summarized [here](docs/COPYRIGHT).