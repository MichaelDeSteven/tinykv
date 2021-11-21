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
	"errors"
	"math/rand"

	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64

	peers []uint64

	voteCnt uint64

	randomElectionTimeout int
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	return &Raft{
		id:               c.ID,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		Prs:              make(map[uint64]*Progress),
		State:            StateFollower,
		msgs:             make([]pb.Message, 0),
		votes:            make(map[uint64]bool),
		peers:            c.peers,
		voteCnt:          0,
		Vote:             None,
	}
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      to,
		MsgType: pb.MessageType_MsgAppend,
		Term:    r.Term,
		Entries: []*pb.Entry{},
	})
	return false
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      to,
		Term:    r.Term,
		MsgType: pb.MessageType_MsgHeartbeat,
	})
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	if r.State == StateFollower || r.State == StateCandidate {
		r.tickElection()
	}
	if r.State == StateLeader {
		r.tickHeartBeat()
	}
}

func (r *Raft) tickElection() {
	r.electionElapsed++
	if r.electionElapsed > r.randomElectionTimeout {
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgHup,
		})
	}
}

func (r *Raft) tickHeartBeat() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed > r.heartbeatTimeout {
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgBeat,
		})
	}
	if r.State == StateLeader {
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgPropose,
		})
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.Term = term
	r.Lead = lead
	r.State = StateFollower
	r.reset()
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.Term++
	r.votes[r.id] = true
	r.Vote = r.id
	r.State = StateCandidate
	r.voteCnt = 1
	r.reset()
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
	r.reset()
}

func (r *Raft) reset() {
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.handleMsgHup(m)
	case pb.MessageType_MsgBeat:
		r.handleMsgBeat(m)
	case pb.MessageType_MsgPropose:
		r.handleMsgPro(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handlerRequestVoteResponse(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	}
	return nil
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// 投票合法性检查
	reject := true
	if r.Term < m.Term {
		r.Vote = None
		r.becomeFollower(m.Term, m.From)
	}
	if r.Term <= m.Term {
		if r.Vote == None || r.Vote == m.From {
			r.Vote = m.From
			reject = false
		}
	}
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      m.From,
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		Term:    m.Term,
		Reject:  reject,
	})
}

func (r *Raft) handlerRequestVoteResponse(m pb.Message) {
	if r.Term == m.Term && r.State == StateCandidate && !m.Reject {
		r.voteCnt++
		if r.voteCnt >= uint64(len(r.peers)>>1)+1 {
			r.becomeLeader()
			// broadcast
		}
	}
}

func (r *Raft) handleMsgHup(m pb.Message) {
	r.becomeCandidate()
	// single node case
	if len(r.peers) == 1 {
		r.becomeLeader()
	}
	// muti node case
	for _, id := range r.peers {
		if id == r.id {
			continue
		}
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      id,
			MsgType: pb.MessageType_MsgRequestVote,
			Term:    r.Term,
		})
	}
}

func (r *Raft) handleMsgBeat(m pb.Message) {
	r.becomeFollower(m.Term, m.From)
	for _, id := range r.peers {
		if id == r.id {
			continue
		}
		r.sendHeartbeat(id)
	}
}

func (r *Raft) handleMsgPro(m pb.Message) {
	if r.State == StateLeader {
		for _, id := range r.peers {
			if id == r.id {
				continue
			}
			r.sendHeartbeat(id)
		}
	}
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		r.becomeFollower(m.Term, m.From)
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      m.From,
			MsgType: pb.MessageType_MsgHeartbeatResponse,
		})
	} else {
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			MsgType: pb.MessageType_MsgHeartbeatResponse,
			Reject:  true,
		})
	}
}

func (r *Raft) bcastAppend(m pb.Message) {
	for _, id := range r.peers {
		if id == r.id {
			continue
		}
		r.sendAppend(id)
	}
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		r.becomeFollower(m.Term, m.From)
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      m.From,
			MsgType: pb.MessageType_MsgHeartbeatResponse,
		})
	} else {
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			MsgType: pb.MessageType_MsgHeartbeatResponse,
			Reject:  true,
		})
	}
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}
