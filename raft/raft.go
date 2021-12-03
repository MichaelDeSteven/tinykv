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

	randomElectionTimeout int
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	
	hardState, _, err := c.Storage.InitialState()
	if err != nil {
		panic(err)
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
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	ents, err := r.RaftLog.slice(r.Prs[to].Next, r.RaftLog.LastIndex()+1)
	if err != nil {
		panic(err)
	}
	es := make([]*pb.Entry, len(ents))
	for i := 0; i < len(ents); i++ {
		es[i] = &ents[i]
	}
	log.Printf("[sendAppend] leader: %d, send ents{%+v} to Follower：%d\n", r.id, es, to)
	prevLogIndex := r.Prs[to].Next - 1
	prevLogTerm, err := r.RaftLog.Term(prevLogIndex)
	if err != nil {
		panic(err)
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
	r.msgs = append(r.msgs, pb.Message{
		From:    r.id,
		To:      to,
		Term:    r.Term,
		MsgType: pb.MessageType_MsgHeartbeat,
		Commit:  r.RaftLog.committed,
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
		r.heartbeatElapsed = 0
		log.Printf("[tickHeartBeat] leader：%d, send MsgBeat\n", r.id)
		r.Step(pb.Message{
			From:    r.id,
			To:      r.id,
			MsgType: pb.MessageType_MsgBeat,
			Term:    r.Term,
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
	r.State = StateCandidate
	r.reset()
	r.Vote = r.id
	r.votes[r.id] = true
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
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

func (r *Raft) resetVotes() {
	r.votes = map[uint64]bool{}
}

func (r *Raft) reset() {
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	r.resetVotes()
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
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResponse(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResponse(m)
	}
	return nil
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// check vote rule
	reject := true
	if r.Term > m.Term {
		// ignored
		return
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

// 5.4.1 election restriction
func (r *Raft) isMoreUpToDate(lastLogTerm, lastLogIndex uint64) bool {
	li := r.RaftLog.LastIndex()
	liTerm, err := r.RaftLog.Term(li)
	log.Printf("liTerm：%d, li：%d\n", liTerm, li)
	log.Printf("lastLogTerm：%d, lastLogIndex：%d\n", lastLogTerm, lastLogIndex)
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

func (r *Raft) handlerRequestVoteResponse(m pb.Message) {
	if r.Term == m.Term && r.State == StateCandidate {
		r.recordVote(m.From, !m.Reject)
		granted, rejected := r.cntVote()
		log.Printf("[handlerRequestVoteResponse] granted：%d, rejected：%d\n", granted, rejected)
		if granted > (len(r.Prs) >> 1) {
			r.becomeLeader()
		} else if rejected > (len(r.Prs) >> 1) {
			r.becomeFollower(m.Term, None)
		}
	}
	// TODO m.Term > r.Term case
}

func (r *Raft) handleMsgHup(m pb.Message) {
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
			prevLogIndex := r.RaftLog.LastIndex()
			prevLogTerm, err := r.RaftLog.Term(prevLogIndex)
			if err != nil {
				panic(err)
			}
			r.msgs = append(r.msgs, pb.Message{
				From:    r.id,
				To:      k,
				MsgType: pb.MessageType_MsgRequestVote,
				Term:    r.Term,
				Index:   prevLogIndex,
				LogTerm: prevLogTerm,
			})
		}
	}
}

func (r *Raft) handleMsgBeat(m pb.Message) {
	if r.State == StateLeader {
		for k, _ := range r.Prs {
			if k == r.id {
				continue
			}
			r.sendHeartbeat(k)
		}
	}
}

func (r *Raft) handleMsgPro(m pb.Message) {
	if r.State == StateLeader {
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
		log.Printf("[handleMsgPro] drop entry\n")
	}
}

func (r *Raft) appendEntries(entries []*pb.Entry) {
	ents := make([]pb.Entry, 0)
	li := r.RaftLog.LastIndex()
	for i, e := range entries {
		e.Index = li + 1 + uint64(i)
		e.Term = r.Term
		log.Printf("[appendEntries] {%+v}\n", e)
		ents = append(ents, *e)
	}
	r.RaftLog.append(ents...)
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	// log.Printf("Next:%d, Match:%d\n", r.Prs[r.id].Next, r.Prs[r.id].Match)
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		r.becomeFollower(m.Term, m.From)
		if m.Index < r.RaftLog.committed {
			r.msgs = append(r.msgs, pb.Message{
				From:    r.id,
				To:      m.From,
				Term:    m.Term,
				MsgType: pb.MessageType_MsgAppendResponse,
			})
			return
		}
		log.Printf("[handleAppendEntries] %+v\n", m)
		// follower log Index greater than leader prevLogIndex
		if m.Index > r.RaftLog.LastIndex() {
			prevLogTerm, _ := r.RaftLog.Term(r.RaftLog.LastIndex())
			r.msgs = append(r.msgs, pb.Message{
				From:    r.id,
				To:      m.From,
				Term:    r.Term,
				MsgType: pb.MessageType_MsgAppendResponse,
				Index:   r.RaftLog.LastIndex(),
				LogTerm: prevLogTerm,
				Reject:  true,
			})
			return
		}

		// unmatch：decrease prevlogIndex to retry
		prevLogTerm, _ := r.RaftLog.Term(m.Index)
		if prevLogTerm != m.LogTerm {
			prevLogTerm, _ := r.RaftLog.Term(m.Index - 1)
			r.msgs = append(r.msgs, pb.Message{
				From:    r.id,
				To:      m.From,
				Term:    r.Term,
				MsgType: pb.MessageType_MsgAppendResponse,
				Index:   m.Index - 1,
				LogTerm: prevLogTerm,
				Reject:  true,
			})
			return
		}
		// log.Printf("prevLogTerm: %d, m.LogTerm: %d, m.Index: %d\n", prevLogTerm, m.LogTerm, m.Index)
		// Log Matching property
		if len(m.Entries) > 0 {
			i, j := 0, m.Index+1
			for i < len(m.Entries) && j <= r.RaftLog.LastIndex() {
				prevLogTerm, err := r.RaftLog.Term(j)
				if err != nil {
					panic(err)
				}
				if prevLogTerm != m.Entries[i].Term {
					r.RaftLog.stabled = min(r.RaftLog.stabled, j-1)
					if offset := r.RaftLog.offset; j-offset < uint64(len(r.RaftLog.entries)) {
						r.RaftLog.entries = r.RaftLog.entries[:j-offset]
					}
					break
				}
				i, j = i+1, j+1
			}
			if i < len(m.Entries) {
				for _, e := range m.Entries[i:] {
					r.RaftLog.append(*e)
				}
			}
		}

		// leaderCommit > commitIndex case
		if m.Commit > r.RaftLog.committed {
			// log.Printf("%d, %d\n", r.RaftLog.LastIndex(), m.Commit)
			li := m.Index
			if len(m.Entries) > 0 {
				li = m.Entries[len(m.Entries)-1].Index
			}
			r.RaftLog.committed = min(m.Commit, li)
		}
		logTerm, _ := r.RaftLog.Term(m.Index)
		r.msgs = append(r.msgs, pb.Message{
			From:    r.id,
			To:      m.From,
			MsgType: pb.MessageType_MsgAppendResponse,
			Term:    m.Term,
			Index:   r.RaftLog.LastIndex(),
			LogTerm: logTerm,
		})
	} else {
		// ignored
		log.Printf("[handleAppendEntries] term < currentTerm\n")
	}
}

func (r *Raft) handleAppendResponse(m pb.Message) {
	if r.Term > m.Term {
		log.Printf("%+v\n", m)
		log.Printf("[handleAppendResponse] drop: term > m.Term\n")
		return
	}
	if r.Term < m.Term {
		log.Printf("[TODO][handleAppendResponse] term < m.Term\n")
		return
	}
	if r.Term == m.Term {
		log.Printf("[handleAppendResponse] leader: %d, append response ent{%+v}\n", r.id, r.RaftLog.entries)
		if m.Reject {
			r.Prs[m.From].Next = m.Index + 1
			log.Printf("[handleAppendResponse] follower: %d Reject, Index: %d", m.From, m.Index)
			// TODO compare prevlogIndex prevlogTerm
			if r.Prs[m.From].Next < 0 {
				log.Printf("[TODO][handleAppendResponse] r.Prs[m.From].Next < 0\n")
				return
			}
			r.sendAppend(m.From)
		} else {
			r.Prs[m.From].Match = m.Index
			r.Prs[m.From].Next = m.Index + 1
			log.Printf("[handleAppendResponse] leader: %d, follower：%d accepted, index：%d\n", r.id, m.From, m.Index)
			if r.maybeCommit() {
				log.Printf("[handleAppendResponse] leader: %d, index %d committed\n", r.id, r.RaftLog.committed)
				r.bcastAppend()
			}
		}
	}
}

func (r *Raft) maybeCommit() bool {
	for i := r.RaftLog.committed + 1; i <= r.RaftLog.LastIndex(); i++ {
		cnt := 0
		for _, prs := range r.Prs {
			if prs.Match >= i {
				cnt++
			}
		}
		term, err := r.RaftLog.Term(i)
		if err != nil {
			panic(err)
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

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term >= r.Term {
		if r.State == StateLeader {
			log.Printf("[!!!TODO][handleHeartbeat] State == StateLeader\n")
			return
		}
		if r.State == StateCandidate {
			if m.Term > r.Term {
				r.becomeFollower(m.Term, m.From)
			} else {
				log.Printf("[!!!TODO][handleHeartbeat] State == StateCandidate, m.Term == r.Term\n")
				return
			}
		}
		if r.State == StateFollower {
			if m.Term > r.Term {
				r.Lead = m.From
			}
			if r.RaftLog.committed < m.Commit {
				r.RaftLog.committed = m.Commit
				r.msgs = append(r.msgs, pb.Message{
					From:    r.id,
					To:      m.From,
					Term:    r.Term,
					MsgType: pb.MessageType_MsgHeartbeatResponse,
				})
			}
		}
	} else {
		log.Printf("[!!!TODO][handleHeartbeat] term < currentTerm\n")
	}
}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if r.State == StateLeader {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, None)
		} else {
			r.sendAppend(m.From)
		}
	} else {
		log.Printf("[TODO][handleHeartbeatResponse] State != StateLeader\n")
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
