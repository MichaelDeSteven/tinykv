// Copyright 2015 The etcd Authors
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

package raft

import (
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// RaftLog manage the log entries, its struct look like:
//
//  snapshot/first.....applied....committed....stabled.....last
//  --------|------------------------------------------------|
//                            log entries
//
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot

	// Your Data Here (2A).

	offset uint64
}

// newLog returns log using the given storage. It recovers the log
// to the state that it just commits and applies the latest snapshot.
func newLog(storage Storage) *RaftLog {
	// Your Code Here (2A).
	firstIndex, err := storage.FirstIndex()
	if err != nil {
		panic(err)
	}
	lastIndex, err := storage.LastIndex()
	if err != nil {
		panic(err)
	}
	DPrintf("[newLog] firstIndex: %d, lastIndex: %d\n", firstIndex, lastIndex)
	ents, err := storage.Entries(firstIndex, lastIndex+1)
	// if err != nil {
	// 	panic(err)
	// }

	return &RaftLog{
		committed: firstIndex - 1,
		applied:   firstIndex - 1,
		storage:   storage,
		entries:   ents,
		stabled:   lastIndex,
		offset:    firstIndex,
	}
}

// We need to compact the log entries in some point of time like
// storage compact stabled log entries prevent the log entries
// grow unlimitedly in memory
func (l *RaftLog) maybeCompact() {
	// Your Code Here (2C).
	if len(l.entries) > 0 {
		fi, _ := l.storage.FirstIndex()
		DPrintf("[maybeCompact] fi: %d, offset: %d\n", fi, l.offset)
		if fi > l.offset {
			if len(l.entries) > 0 {
				l.entries = l.entries[fi-l.offset:]
				ents := make([]pb.Entry, len(l.entries))
				copy(ents, l.entries)
				l.entries = ents
			}
			l.offset = fi
		}
	}

}

// unstableEntries return all the unstable entries
func (l *RaftLog) unstableEntries() []pb.Entry {
	// Your Code Here (2A).
	if l.LastIndex() <= l.stabled {
		return []pb.Entry{}
	}
	es, err := l.slice(l.stabled+1, l.LastIndex()+1)
	if err != nil {
		panic(err)
	}
	return es
}

// nextEnts returns all the committed but not applied entries
func (l *RaftLog) nextEnts() (ents []pb.Entry) {
	// Your Code Here (2A).
	if l.applied+1 > l.committed {
		return []pb.Entry{}
	}
	es, err := l.slice(l.applied+1, min(l.committed+1, uint64(len(l.entries))+l.offset))
	if err != nil {
		panic(err)
	}
	return es
}

// LastIndex return the last index of the log entries
func (l *RaftLog) LastIndex() uint64 {
	// Your Code Here (2A).
	if entsLen := len(l.entries); entsLen != 0 {
		return l.entries[entsLen-1].Index
	}
	li, err := l.storage.FirstIndex()
	if err != nil {
		panic(err)
	}
	return li - 1
}

// Term return the term of the entry in the given index
func (l *RaftLog) Term(i uint64) (uint64, error) {
	// Your Code Here (2A).
	if i > l.LastIndex() {
		return 0, ErrUnavailable
	}
	if i >= l.offset {
		return l.entries[i-l.offset].Term, nil
	}
	return l.storage.Term(i)
}

func (l *RaftLog) append(entries ...pb.Entry) {
	l.entries = append(l.entries, entries...)
}

func (l *RaftLog) FirstIndex() uint64 {
	if l.pendingSnapshot != nil {
		return l.pendingSnapshot.Metadata.Index + 1
	} else {
		index, err := l.storage.FirstIndex()
		if err != nil {
			panic(err)
		}
		return index
	}
}

// l.firstIndex <= lo <= hi <= l.firstIndex + len(l.entries)
func (l *RaftLog) checkOutOfBounds(lo, hi uint64) error {
	if lo > hi {
		panic(ErrUnexpectedCondition)
	}
	fi := l.FirstIndex()
	if lo < fi {
		DPrintf("[checkOutOfBounds] lo: %d, fi: %d\n", lo, fi)
		return ErrCompacted
	}

	length := l.LastIndex() + 1 - fi
	if hi > fi+length {
		panic(ErrUnexpectedCondition)
	}
	return nil
}

// get the index of [lo, hi) entries
func (l *RaftLog) slice(lo, hi uint64) ([]pb.Entry, error) {
	DPrintf("[slice] index[%d:%d)\n", lo, hi)
	err := l.checkOutOfBounds(lo, hi)
	if err != nil {
		return nil, err
	}
	if lo == hi {
		return nil, nil
	}
	if lo >= l.offset && hi <= l.offset+uint64(len(l.entries)) {
		return l.entries[lo-l.offset : hi-l.offset], nil
	}
	return l.storage.Entries(lo, hi)
}

func (l *RaftLog) Snapshot() (pb.Snapshot, error) {
	if l.pendingSnapshot != nil {
		return *l.pendingSnapshot, nil
	}
	return l.storage.Snapshot()
}
