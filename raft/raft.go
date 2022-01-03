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
	"log"
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

const Debug = 1

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

var ErrUnexpectedCondition = errors.New("Unexpected Condition")

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
	Match, Next  uint64
	recentActive bool
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

	randomElectionTimeout int
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	hardState, confState, err := c.Storage.InitialState()
	if err != nil {
		panic(err)
	}
	if c.peers == nil {
		c.peers = confState.Nodes
	}
	prs := make(map[uint64]*Progress)
	for _, p := range c.peers {
		prs[p] = &Progress{
			Next:  0,
			Match: 0,
		}
	}
	r := &Raft{
		id:               c.ID,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		Prs:              prs,
		State:            StateFollower,
		msgs:             make([]pb.Message, 0),
		votes:            make(map[uint64]bool),
		RaftLog:          newLog(c.Storage),
	}

	r.Vote = hardState.Vote
	r.Term = hardState.Term
	r.RaftLog.committed = hardState.Commit
	r.becomeFollower(r.Term, None)
	DPrintf("[newRaft] %+v\n", r)
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	DPrintf("[sendAppend] %d sendAppend To %d, next: %d, last: %d\n", r.id, to, r.Prs[to].Next, r.RaftLog.LastIndex())
	if r.Prs[to].Next > r.RaftLog.LastIndex()+1 {
		return true
	}
	// if a leader fails to get term or entries, the leader requests snapshot
	ents, erre := r.RaftLog.slice(r.Prs[to].Next, r.RaftLog.LastIndex()+1)
	prevLogIndex := r.Prs[to].Next - 1
	prevLogTerm, errt := r.RaftLog.Term(prevLogIndex)
	if erre != nil || errt != nil {
		DPrintf("[sendAppend] send snapshot\n")
		snapshot, err := r.RaftLog.Snapshot()
		if err != nil {
			if err == ErrSnapshotTemporarilyUnavailable {
				DPrintf("[sendAppend] temporarily unavailable\n")
				return false
			}
			panic(ErrUnexpectedCondition)
		}
		if IsEmptySnap(&snapshot) {
			panic("need non-empty snapshot")
		}
		r.msgs = append(r.msgs, pb.Message{
			MsgType:  pb.MessageType_MsgSnapshot,
			From:     r.id,
			To:       to,
			Term:     r.Term,
			Snapshot: &snapshot,
		})
		r.Prs[to].Next = snapshot.Metadata.Index + 1
		return false
	}
	es := make([]*pb.Entry, len(ents))
	for i := 0; i < len(ents); i++ {
		es[i] = &ents[i]
	}
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      to,
		MsgType: pb.MessageType_MsgAppend,
		Term:    r.Term,
		Entries: es,
		Commit:  r.RaftLog.committed,
		Index:   prevLogIndex,
		LogTerm: prevLogTerm,
	})
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	DPrintf("[sendHeartbeat] %d sendHeartbeat to %d\n", r.id, to)
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      to,
		Term:    r.Term,
		MsgType: pb.MessageType_MsgHeartbeat,
		// Commit:  r.RaftLog.committed,
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
	if r.electionElapsed >= r.randomElectionTimeout {
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgHup,
		})
	}
}

func (r *Raft) tickHeartBeat() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		if !r.checkQuorumActive() {
			r.becomeFollower(r.Term, None)
			return
		}
		r.heartbeatElapsed = 0
		DPrintf("[tickHeartBeat] leader：%d, send MsgBeat\n", r.id)
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgBeat,
			Term:    r.Term,
		})
	}
}

func (r *Raft) checkQuorumActive() bool {
	if pr := r.Prs[r.id]; pr != nil {
		pr.recentActive = true
	}
	cnt := 0
	for _, prs := range r.Prs {
		if prs.recentActive {
			cnt++
		}
		prs.recentActive = false
	}
	if cnt < (len(r.Prs) >> 1) {
		return false
	}
	return true
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
	r.State = StateCandidate
	r.reset()
	r.Vote = r.id
	r.votes[r.id] = true
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	DPrintf("[becomeLeader] %d become Leader\n", r.id)
	r.State = StateLeader
	r.Lead = r.id
	r.reset()

	li := r.RaftLog.LastIndex()
	for i, prs := range r.Prs {
		prs.Match = 0
		prs.Next = li + 1
		if i == r.id {
			prs.Match = li
			prs.Next = li + 1
		}
	}

	// noop entry
	r.appendEntries([]*pb.Entry{{Data: nil}})
	r.bcastAppend()

	// single node case
	if len(r.Prs) == 1 {
		r.maybeCommit()
	}
}

func (r *Raft) reset() {
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	// randomElectionTimeout in [electionTimeout, 2*electionTimeout)
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.votes = map[uint64]bool{}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.hup(m)
	case pb.MessageType_MsgBeat:
		r.msgBeat(m)
	case pb.MessageType_MsgPropose:
		r.propose(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handlerRequestVoteResponse(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResponse(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendEntriesResponse(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	case pb.MessageType_MsgTimeoutNow:
		r.handleTimeoutNow(m)
	}
	return nil
}

func (r *Raft) hup(m pb.Message) {
	if r.State != StateLeader {
		r.becomeCandidate()
		// single node case
		if len(r.Prs) == 1 {
			r.becomeLeader()
			return
		}
		// muti node case
		for k, _ := range r.Prs {
			if k == r.id {
				continue
			}
			lastLogIndex := r.RaftLog.LastIndex()
			lastLogTerm, err := r.RaftLog.Term(lastLogIndex)
			if err != nil {
				panic(err)
			}
			r.msgs = append(r.msgs, pb.Message{
				From:    r.id,
				To:      k,
				MsgType: pb.MessageType_MsgRequestVote,
				Term:    r.Term,
				Index:   lastLogIndex,
				LogTerm: lastLogTerm,
			})
		}
	}
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// check vote rule
	reject := true
	if r.Term > m.Term {
		// reject
	} else {
		if !r.isMoreUpToDate(m.LogTerm, m.Index) {
			r.becomeFollower(m.Term, None)
		} else {
			if m.Term > r.Term || r.Vote == None || r.Vote == m.From {
				r.becomeFollower(m.Term, None)
				r.Vote = m.From
				reject = false
			}
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
	if r.Term == m.Term && r.State == StateCandidate {
		majority := len(r.Prs) >> 1
		r.recordVote(m.From, !m.Reject)
		granted, rejected := r.cntVote()
		DPrintf("[handlerRequestVoteResponse] Candidates %d: granted：%d, rejected：%d\n", r.id, granted, rejected)
		if granted > majority {
			r.becomeLeader()
		} else if rejected > majority {
			r.becomeFollower(m.Term, None)
		}
	}
	// TODO m.Term > r.Term case
}

// 5.4.1 election restriction
func (r *Raft) isMoreUpToDate(lastLogTerm, lastLogIndex uint64) bool {
	li := r.RaftLog.LastIndex()
	liTerm, err := r.RaftLog.Term(li)
	DPrintf("liTerm：%d, li：%d\n", liTerm, li)
	DPrintf("lastLogTerm：%d, lastLogIndex：%d\n", lastLogTerm, lastLogIndex)
	if err != nil {
		panic(liTerm)
	}
	if lastLogTerm > liTerm || (lastLogTerm == liTerm && lastLogIndex >= li) {
		return true
	}
	return false
}

func (r *Raft) recordVote(id uint64, v bool) {
	_, ok := r.votes[id]
	if !ok {
		r.votes[id] = v
	}
}

func (r *Raft) cntVote() (granted, rejected int) {
	for id, _ := range r.votes {
		v, voted := r.votes[id]
		if !voted {
			continue
		}
		if v {
			granted++
		} else {
			rejected++
		}
	}
	return granted, rejected
}

func (r *Raft) propose(m pb.Message) {
	if r.State == StateLeader {
		if r.leadTransferee != None {
			return
		}
		if len(m.Entries) > 0 && m.Entries[0].EntryType == pb.EntryType_EntryConfChange {
			if r.PendingConfIndex <= r.RaftLog.applied {
				r.PendingConfIndex = r.RaftLog.applied
			} else {
				r.msgs = append(r.msgs, m)
				return
			}
		}
		r.appendEntries(m.Entries)
		// single node case
		if len(r.Prs) == 1 {
			r.maybeCommit()
		} else {
			r.bcastAppend()
		}
	}
	if r.State == StateFollower {
		m.From = r.id
		m.To = r.Lead
		r.msgs = append(r.msgs, m)
	}
	if r.State == StateCandidate {
		DPrintf("[handleMsgPro] Candidate drop entry\n")
	}
}

func (r *Raft) appendEntries(entries []*pb.Entry) {
	ents := make([]pb.Entry, 0)
	li := r.RaftLog.LastIndex()
	for i, e := range entries {
		e.Index = li + 1 + uint64(i)
		e.Term = r.Term
		DPrintf("[appendEntries] {%+v}\n", e)
		ents = append(ents, *e)
	}
	r.RaftLog.append(ents...)
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		r.becomeFollower(m.Term, m.From)
		// follower log Index greater than leader prevLogIndex
		if m.Index > r.RaftLog.LastIndex() {
			r.sendAppendResponse(m.From, true)
			return
		}

		// unmatch：decrease prevlogIndex to retry
		prevLogTerm, err := r.RaftLog.Term(m.Index)
		if err != nil || prevLogTerm != m.LogTerm {
			r.sendAppendResponse(m.From, true)
			return
		}
		// Log Matching property
		i, j := 0, m.Index+1
		for i < len(m.Entries) && j <= r.RaftLog.LastIndex() {
			prevLogTerm, _ := r.RaftLog.Term(j)
			// if err != nil {
			// 	panic(err)
			// }
			// if existing entry conflicts, delete the existing entry and all that follow it
			if prevLogTerm != m.Entries[i].Term {
				if j < r.RaftLog.offset {
					r.RaftLog.entries = r.RaftLog.entries[:0]
				} else {
					r.RaftLog.entries = r.RaftLog.entries[:j-r.RaftLog.offset]
				}
				r.RaftLog.stabled = min(r.RaftLog.stabled, j-1)
				break
			}
			i, j = i+1, j+1
		}
		if i < len(m.Entries) {
			for _, e := range m.Entries[i:] {
				r.RaftLog.append(*e)
			}
		}

		// If leaderCommit > commitIndex, set commitIndex = min(leaderCommit, index of last new entry)
		if m.Commit > r.RaftLog.committed {
			li := m.Index
			if len(m.Entries) > 0 {
				li = m.Entries[len(m.Entries)-1].Index
			}
			r.RaftLog.committed = min(m.Commit, li)
		}
		r.sendAppendResponse(m.From, false)
	} else {
		r.sendAppendResponse(m.From, true)
	}
}

func (r *Raft) handleAppendEntriesResponse(m pb.Message) {
	if r.State != StateLeader {
		return
	}
	if r.Term > m.Term {
		DPrintf("[handleAppendResponse] drop: term > m.Term\n")
		return
	}
	if r.Term < m.Term {
		r.becomeFollower(m.Term, None)
		return
	}
	if r.Term == m.Term {
		r.Prs[m.From].recentActive = true
		if m.Reject {
			DPrintf("[handleAppendResponse] follower: %d Reject, Index: %d", m.From, m.Index)
			if r.Prs[m.From].Next > 0 {
				r.Prs[m.From].Next -= 1
				r.sendAppend(m.From)
			}
		} else {
			r.Prs[m.From].Match = m.Index
			r.Prs[m.From].Next = m.Index + 1
			r.maybeTransferLeader()
			DPrintf("[handleAppendResponse] leader: %d, follower：%d accepted, index：%d\n", r.id, m.From, m.Index)
		}
		if r.maybeCommit() {
			DPrintf("[handleAppendResponse] leader: %d, index %d was committed\n", r.id, r.RaftLog.committed)
			r.bcastAppend()
		}
	}
}

func (r *Raft) sendAppendResponse(to uint64, reject bool) {
	li := r.RaftLog.LastIndex()
	logTerm, err := r.RaftLog.Term(li)
	if err != nil {
		panic(ErrUnexpectedCondition)
	}
	msg := pb.Message{
		From:    r.id,
		To:      to,
		MsgType: pb.MessageType_MsgAppendResponse,
		Term:    r.Term,
		Index:   li,
		LogTerm: logTerm,
		Reject:  reject,
	}
	DPrintf("[sendAppendResponse] %+v\n", msg)
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) maybeCommit() bool {
	for i := r.RaftLog.LastIndex(); i > r.RaftLog.committed; i-- {
		cnt := 0
		for _, prs := range r.Prs {
			if prs.Match >= i {
				cnt++
			}
		}
		term, err := r.RaftLog.Term(i)
		if err != nil {
			continue
			// panic(err)
		}
		if cnt > (len(r.Prs)>>1) && r.Term == term {
			r.RaftLog.committed = i
			return true
		}
	}
	return false
}

func (r *Raft) bcastAppend() {
	for k, _ := range r.Prs {
		if k == r.id {
			continue
		}
		r.sendAppend(k)
	}
}

func (r *Raft) msgBeat(m pb.Message) {
	if r.State == StateLeader {
		for k, _ := range r.Prs {
			if k == r.id {
				continue
			}
			DPrintf("[msgBeat] leader：%d send heartbeat\n", r.id)
			r.sendHeartbeat(k)
		}
	} else {
		// ignore
	}
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		r.becomeFollower(m.Term, m.From)
		r.RaftLog.committed = min(m.Commit, r.RaftLog.committed)
	}
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
		MsgType: pb.MessageType_MsgHeartbeatResponse,
	})
}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if r.State == StateLeader {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, None)
		} else {
			r.Prs[m.From].recentActive = true
			r.sendAppend(m.From)
		}
	} else {
		// ignore
	}
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	DPrintf("[handleSnapshot] %+v\n", m.Snapshot)
	if m.Term < r.Term {
		return
	}
	r.becomeFollower(m.Term, m.From)
	snapshot, l := m.Snapshot, r.RaftLog
	sindex, sterm := snapshot.Metadata.Index, snapshot.Metadata.Term
	if sindex < r.RaftLog.committed {
		r.sendAppendResponse(m.From, true)
		return
	}
	r.RaftLog.pendingSnapshot = snapshot
	ents := make([]pb.Entry, 1)
	ents[0].Index = sindex
	ents[0].Term = sterm
	l.entries = ents
	l.offset = sindex
	l.committed = sindex
	l.applied = sindex
	l.stabled = sindex
	// restore node information
	nodes := m.Snapshot.Metadata.ConfState.Nodes
	prs := make(map[uint64]*Progress)
	for _, i := range nodes {
		prs[i] = &Progress{Next: r.RaftLog.LastIndex(), Match: 0}
	}
	r.Prs = prs
	r.sendAppendResponse(m.From, false)
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
	DPrintf("[addNode] add %d\n", id)
	r.Prs[id] = &Progress{Match: 0, Next: r.RaftLog.LastIndex() + 1}
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
	DPrintf("[removeNode] remove %d\n", id)
	delete(r.Prs, id)
	r.maybeCommit()
}

func (r *Raft) checkQualification(transferee uint64) bool {
	if _, ok := r.Prs[transferee]; !ok || transferee == r.id || r.leadTransferee != None {
		DPrintf("[checkQualification] ingore\n")
		return false
	}
	r.leadTransferee = transferee
	// check is up to date
	DPrintf("[checkQualification] %d, %d\n", r.Prs[transferee].Match, r.Prs[transferee].Next)
	if prs, _ := r.Prs[transferee]; prs.Match < r.RaftLog.LastIndex() {
		r.sendAppend(transferee)
		return false
	}
	return true
}

func (r *Raft) transferLeader() {
	DPrintf("[transferLeader] leader: %d, send MsgTimeoutNow\n", r.id)
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      r.leadTransferee,
		MsgType: pb.MessageType_MsgTimeoutNow,
	})
	r.leadTransferee = None
}

func (r *Raft) handleTransferLeader(m pb.Message) {
	if r.State == StateLeader {
		DPrintf("[handleTransferLeader] %+v\n", m)
		if r.checkQualification(m.From) {
			r.transferLeader()
		}
	} else {
		DPrintf("[handleTransferLeader] redirect to leader\n")
		m.To = r.Lead
		r.msgs = append(r.msgs, m)
	}
}

func (r *Raft) maybeTransferLeader() {
	if prs := r.Prs[r.leadTransferee]; r.leadTransferee != None && prs.Match+1 == prs.Next {
		r.transferLeader()
	}
}

func (r *Raft) handleTimeoutNow(m pb.Message) {
	DPrintf("[handleTimeoutNow] %d start a new election\n", r.id)
	if _, ok := r.Prs[r.id]; !ok {
		return
	}
	r.Step(pb.Message{From: r.id, To: r.id, MsgType: pb.MessageType_MsgHup})
}
