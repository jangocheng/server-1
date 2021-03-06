package paxos

import (
	"encoding/binary"
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	mdb "github.com/msackman/gomdb"
	mdbs "github.com/msackman/gomdb/server"
	"goshawkdb.io/common"
	"goshawkdb.io/server"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/configuration"
	"goshawkdb.io/server/db"
	"goshawkdb.io/server/dispatcher"
	eng "goshawkdb.io/server/txnengine"
	"log"
)

func init() {
	db.DB.Proposers = &mdbs.DBISettings{Flags: mdb.CREATE}
}

const ( //                  txnId  rmId
	instanceIdPrefixLen = common.KeyLen + 4
)

type instanceIdPrefix [instanceIdPrefixLen]byte

type ProposerManager struct {
	ServerConnectionPublisher
	RMId          common.RMId
	BootCount     uint32
	VarDispatcher *eng.VarDispatcher
	Exe           *dispatcher.Executor
	DB            *db.Databases
	proposals     map[instanceIdPrefix]*proposal
	proposers     map[common.TxnId]*Proposer
	topology      *configuration.Topology
}

func NewProposerManager(exe *dispatcher.Executor, rmId common.RMId, cm ConnectionManager, db *db.Databases, varDispatcher *eng.VarDispatcher) *ProposerManager {
	pm := &ProposerManager{
		ServerConnectionPublisher: NewServerConnectionPublisherProxy(exe, cm),
		RMId:          rmId,
		BootCount:     cm.BootCount(),
		proposals:     make(map[instanceIdPrefix]*proposal),
		proposers:     make(map[common.TxnId]*Proposer),
		VarDispatcher: varDispatcher,
		Exe:           exe,
		DB:            db,
		topology:      nil,
	}
	exe.Enqueue(func() { pm.topology = cm.AddTopologySubscriber(eng.ProposerSubscriber, pm) })
	return pm
}

func (pm *ProposerManager) loadFromData(txnId *common.TxnId, data []byte) error {
	if _, found := pm.proposers[*txnId]; !found {
		proposer, err := ProposerFromData(pm, txnId, data, pm.topology)
		if err != nil {
			return err
		}
		pm.proposers[*txnId] = proposer
		proposer.Start()
	}
	return nil
}

func (pm *ProposerManager) TopologyChanged(topology *configuration.Topology, done func(bool)) {
	resultChan := make(chan struct{})
	enqueued := pm.Exe.Enqueue(func() {
		pm.topology = topology
		for _, proposer := range pm.proposers {
			proposer.TopologyChange(topology)
		}
		close(resultChan)
		done(true)
	})
	if enqueued {
		go pm.Exe.WithTerminatedChan(func(terminated chan struct{}) {
			select {
			case <-resultChan:
			case <-terminated:
				select {
				case <-resultChan:
				default:
					done(false)
				}
			}
		})
	} else {
		done(false)
	}
}

func (pm *ProposerManager) ImmigrationReceived(txn *eng.TxnReader, varCaps *msgs.Var_List, stateChange eng.TxnLocalStateChange) {
	eng.ImmigrationTxnFromCap(pm.Exe, pm.VarDispatcher, stateChange, pm.RMId, txn, varCaps)
}

func (pm *ProposerManager) TxnReceived(sender common.RMId, txn *eng.TxnReader) {
	// Due to failures, we can actually receive outcomes (2Bs) first,
	// before we get the txn to vote on it - due to failures, other
	// proposers will have created abort proposals on our behalf, and
	// consensus may have already been reached. If this is the case, it
	// is correct to ignore this message.
	txnId := txn.Id
	txnCap := txn.Txn
	if _, found := pm.proposers[*txnId]; !found {
		server.Log(txnId, "Received")
		accept := true
		if pm.topology != nil {
			accept = (pm.topology.Next() == nil && pm.topology.Version == txnCap.TopologyVersion()) ||
				// Could also do pm.topology.BarrierReached1(sender), but
				// would need to specialise that to rolls rather than
				// topology txns, and it's enforced on the sending side
				// anyway. Once the sender has received the next topology,
				// it'll do the right thing and locally block until it's
				// in barrier1.
				(pm.topology.Next() != nil && pm.topology.Next().Version == txnCap.TopologyVersion())
			if accept {
				_, found := pm.topology.RMsRemoved()[sender]
				accept = !found
				if accept {
					accept = false
					allocations := txn.Txn.Allocations()
					for idx, l := 0, allocations.Len(); idx < l; idx++ {
						alloc := allocations.At(idx)
						rmId := common.RMId(alloc.RmId())
						if rmId == pm.RMId {
							accept = alloc.Active() == pm.BootCount
							break
						}
					}
					if !accept {
						server.Log(txnId, "Aborting received txn as it was submitted for an older version of us so we may have already voted on it.", pm.BootCount)
					}
				} else {
					server.Log(txnId, "Aborting received txn as sender has been removed from topology.", sender)
				}
			} else {
				server.Log(txnId, "Aborting received txn due to non-matching topology.", txnCap.TopologyVersion())
			}
		}
		if accept {
			proposer := NewProposer(pm, txn, ProposerActiveVoter, pm.topology)
			pm.proposers[*txnId] = proposer
			proposer.Start()

		} else {
			acceptors := GetAcceptorsFromTxn(txnCap)
			fInc := int(txnCap.FInc())
			alloc := AllocForRMId(txnCap, pm.RMId)
			ballots := MakeAbortBallots(txn, alloc)
			pm.NewPaxosProposals(txn, fInc, ballots, acceptors, pm.RMId, true)
			// ActiveLearner is right - we don't want the proposer to
			// vote, but it should exist to collect the 2Bs that should
			// come back.
			proposer := NewProposer(pm, txn, ProposerActiveLearner, pm.topology)
			pm.proposers[*txnId] = proposer
			proposer.Start()
		}
	}
}

func (pm *ProposerManager) NewPaxosProposals(txn *eng.TxnReader, fInc int, ballots []*eng.Ballot, acceptors []common.RMId, rmId common.RMId, skipPhase1 bool) {
	instId := instanceIdPrefix([instanceIdPrefixLen]byte{})
	instIdSlice := instId[:]
	txnId := txn.Id
	copy(instIdSlice, txnId[:])
	binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], uint32(rmId))
	if _, found := pm.proposals[instId]; !found {
		server.Log(txnId, "NewPaxos; acceptors:", acceptors, "; instance:", rmId)
		prop := NewProposal(pm, txn, fInc, ballots, rmId, acceptors, skipPhase1)
		pm.proposals[instId] = prop
		prop.Start()
	}
}

func (pm *ProposerManager) AddToPaxosProposals(txnId *common.TxnId, ballots []*eng.Ballot, rmId common.RMId) {
	server.Log(txnId, "Adding ballot to Paxos; instance:", rmId)
	instId := instanceIdPrefix([instanceIdPrefixLen]byte{})
	instIdSlice := instId[:]
	copy(instIdSlice, txnId[:])
	binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], uint32(rmId))
	if prop, found := pm.proposals[instId]; found {
		prop.AddBallots(ballots)
	} else {
		log.Printf("Error: Adding ballot to Paxos, unable to find proposals. %v %v\n", txnId, rmId)
	}
}

// from network
func (pm *ProposerManager) OneBTxnVotesReceived(sender common.RMId, txnId *common.TxnId, oneBTxnVotes *msgs.OneBTxnVotes) {
	server.Log(txnId, "1B received from", sender, "; instance:", common.RMId(oneBTxnVotes.RmId()))
	instId := instanceIdPrefix([instanceIdPrefixLen]byte{})
	instIdSlice := instId[:]
	copy(instIdSlice, txnId[:])
	binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], oneBTxnVotes.RmId())
	if prop, found := pm.proposals[instId]; found {
		prop.OneBTxnVotesReceived(sender, oneBTxnVotes)
	}
	// If not found, it should be safe to ignore - it's just a delayed
	// 1B that we clearly don't need to complete the paxos instances
	// anyway.
}

// from network
func (pm *ProposerManager) TwoBTxnVotesReceived(sender common.RMId, txnId *common.TxnId, txn *eng.TxnReader, twoBTxnVotes *msgs.TwoBTxnVotes) {
	instId := instanceIdPrefix([instanceIdPrefixLen]byte{})
	instIdSlice := instId[:]
	copy(instIdSlice, txnId[:])

	switch twoBTxnVotes.Which() {
	case msgs.TWOBTXNVOTES_FAILURES:
		failures := twoBTxnVotes.Failures()
		server.Log(txnId, "2B received from", sender, "; instance:", common.RMId(failures.RmId()))
		binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], failures.RmId())
		if prop, found := pm.proposals[instId]; found {
			prop.TwoBFailuresReceived(sender, &failures)
		}

	case msgs.TWOBTXNVOTES_OUTCOME:
		binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], uint32(pm.RMId))
		outcome := twoBTxnVotes.Outcome()

		if proposer, found := pm.proposers[*txnId]; found {
			server.Log(txnId, "2B outcome received from", sender, "(known active)")
			proposer.BallotOutcomeReceived(sender, &outcome)
			return
		}

		txnCap := txn.Txn

		alloc := AllocForRMId(txnCap, pm.RMId)

		if alloc.Active() != 0 {
			// We have no record of this, but we were active - we must
			// have died and recovered (or we may have never received
			// this yet - see above - if we were down, other proposers
			// may have started abort proposers). Thus this could be
			// abort (abort proposers out there) or commit (we previously
			// voted, and that vote got recorded, but we have since died
			// and restarted).
			server.Log(txnId, "2B outcome received from", sender, "(unknown active)")

			// There's a possibility the acceptor that sent us this 2B is
			// one of only a few acceptors that got enough 2As to
			// determine the outcome. We must set up new paxos instances
			// to ensure the result is propogated to all. All we need to
			// do is to start a proposal for our own vars. The proposal
			// itself will detect any further absences and take care of
			// them.
			acceptors := GetAcceptorsFromTxn(txnCap)
			server.Log(txnId, "Starting abort proposals with acceptors", acceptors)
			fInc := int(txnCap.FInc())
			ballots := MakeAbortBallots(txn, alloc)
			pm.NewPaxosProposals(txn, fInc, ballots, acceptors, pm.RMId, false)

			proposer := NewProposer(pm, txn, ProposerActiveLearner, pm.topology)
			pm.proposers[*txnId] = proposer
			proposer.Start()
			proposer.BallotOutcomeReceived(sender, &outcome)
		} else {
			// Not active, so we are a learner
			if outcome.Which() == msgs.OUTCOME_COMMIT {
				server.Log(txnId, "2B outcome received from", sender, "(unknown learner)")
				// we must be a learner.
				proposer := NewProposer(pm, txn, ProposerPassiveLearner, pm.topology)
				pm.proposers[*txnId] = proposer
				proposer.Start()
				proposer.BallotOutcomeReceived(sender, &outcome)

			} else {
				// Whilst it's an abort now, at some point in the past it
				// was a commit and as such we received that
				// outcome. However, we must have since died and so lost
				// that state/proposer. We should now immediately reply
				// with a TLC.
				server.Log(txnId, "Sending immediate TLC for unknown abort learner")
				// We have no state here, and if we receive further 2Bs
				// from the repeating sender at the acceptor then we will
				// send further TLCs. So the use of OSS here is correct.
				NewOneShotSender(MakeTxnLocallyCompleteMsg(txnId), pm, sender)
			}
		}

	default:
		panic(fmt.Sprintf("Unexpected 2BVotes type: %v", twoBTxnVotes.Which()))
	}
}

// from network
func (pm *ProposerManager) TxnGloballyCompleteReceived(sender common.RMId, txnId *common.TxnId) {
	if proposer, found := pm.proposers[*txnId]; found {
		server.Log(txnId, "TGC received from", sender, "(proposer found)")
		proposer.TxnGloballyCompleteReceived(sender)
	} else {
		server.Log(txnId, "TGC received from", sender, "(ignored)")
	}
}

// from network
func (pm *ProposerManager) TxnSubmissionAbortReceived(sender common.RMId, txnId *common.TxnId) {
	if proposer, found := pm.proposers[*txnId]; found {
		server.Log(txnId, "TSA received from", sender, "(proposer found)")
		proposer.Abort()
	} else {
		server.Log(txnId, "TSA received from", sender, "(ignored)")
	}
}

// from proposer
func (pm *ProposerManager) TxnFinished(txnId *common.TxnId) {
	delete(pm.proposers, *txnId)
}

// We have an outcome by this point, so we should stop sending proposals.
func (pm *ProposerManager) FinishProposers(txnId *common.TxnId) {
	instId := instanceIdPrefix([instanceIdPrefixLen]byte{})
	instIdSlice := instId[:]
	copy(instIdSlice, txnId[:])
	binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], uint32(pm.RMId))
	if prop, found := pm.proposals[instId]; found {
		delete(pm.proposals, instId)
		abortInstances := prop.FinishProposing()
		for _, rmId := range abortInstances {
			binary.BigEndian.PutUint32(instIdSlice[common.KeyLen:], uint32(rmId))
			if prop, found := pm.proposals[instId]; found {
				delete(pm.proposals, instId)
				prop.FinishProposing()
			}
		}
	}
}

func (pm *ProposerManager) Status(sc *server.StatusConsumer) {
	sc.Emit(fmt.Sprintf("Live proposers: %v", len(pm.proposers)))
	for _, prop := range pm.proposers {
		prop.Status(sc.Fork())
	}
	sc.Emit(fmt.Sprintf("Live proposals: %v", len(pm.proposals)))
	for _, prop := range pm.proposals {
		prop.Status(sc.Fork())
	}
	sc.Join()
}

func GetAcceptorsFromTxn(txnCap msgs.Txn) common.RMIds {
	fInc := int(txnCap.FInc())
	twoFInc := fInc + fInc - 1
	acceptors := make([]common.RMId, twoFInc)
	allocations := txnCap.Allocations()
	idx := 0
	for l := allocations.Len(); idx < l && idx < twoFInc; idx++ {
		alloc := allocations.At(idx)
		acceptors[idx] = common.RMId(alloc.RmId())
	}
	// Danger! For the initial topology txns, there are _not_ twoFInc acceptors
	return acceptors[:idx]
}

func MakeTxnLocallyCompleteMsg(txnId *common.TxnId) []byte {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	tlc := msgs.NewTxnLocallyComplete(seg)
	msg.SetTxnLocallyComplete(tlc)
	tlc.SetTxnId(txnId[:])
	return server.SegToBytes(seg)
}

func MakeTxnSubmissionCompleteMsg(txnId *common.TxnId) []byte {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	tsc := msgs.NewTxnSubmissionComplete(seg)
	msg.SetSubmissionComplete(tsc)
	tsc.SetTxnId(txnId[:])
	return server.SegToBytes(seg)
}

func MakeTxnSubmissionAbortMsg(txnId *common.TxnId) []byte {
	seg := capn.NewBuffer(nil)
	msg := msgs.NewRootMessage(seg)
	tsa := msgs.NewTxnSubmissionAbort(seg)
	msg.SetSubmissionAbort(tsa)
	tsa.SetTxnId(txnId[:])
	return server.SegToBytes(seg)
}

func AllocForRMId(txn msgs.Txn, rmId common.RMId) *msgs.Allocation {
	allocs := txn.Allocations()
	for idx, l := 0, allocs.Len(); idx < l; idx++ {
		alloc := allocs.At(idx)
		if common.RMId(alloc.RmId()) == rmId {
			return &alloc
		}
	}
	return nil
}

func MakeAbortBallots(txn *eng.TxnReader, alloc *msgs.Allocation) []*eng.Ballot {
	actions := txn.Actions(true).Actions()
	actionIndices := alloc.ActionIndices()
	ballots := make([]*eng.Ballot, actionIndices.Len())
	for idx, l := 0, actionIndices.Len(); idx < l; idx++ {
		action := actions.At(int(actionIndices.At(idx)))
		vUUId := common.MakeVarUUId(action.VarId())
		ballots[idx] = eng.NewBallotBuilder(vUUId, eng.AbortDeadlock, nil).ToBallot()
	}
	return ballots
}
