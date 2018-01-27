package txnengine

import (
	"fmt"
	capn "github.com/glycerine/go-capnproto"
	"github.com/go-kit/kit/log"
	sl "github.com/msackman/skiplist"
	"goshawkdb.io/common"
	msgs "goshawkdb.io/server/capnp"
	"goshawkdb.io/server/dispatcher"
	"goshawkdb.io/server/utils"
	"goshawkdb.io/server/utils/status"
	"goshawkdb.io/server/utils/txnreader"
	vc "goshawkdb.io/server/utils/vectorclock"
	"sync/atomic"
	"time"
)

type TxnLocalStateChange interface {
	TxnBallotsComplete(...*Ballot)
	TxnLocallyComplete(*Txn)
	TxnFinished(*Txn)
}

type Txn struct {
	logger       log.Logger
	Id           *common.TxnId
	writes       []*common.VarUUId
	localActions []localAction
	voter        bool
	TxnReader    *txnreader.TxnReader
	exe          *dispatcher.Executor
	vd           *VarDispatcher
	stateChange  TxnLocalStateChange
	txnDetermineLocalBallots
	txnAwaitLocalBallots
	txnReceiveOutcome
	txnAwaitLocallyComplete
	txnReceiveCompletion
	currentState txnStateMachineComponent
}

type localAction struct {
	*Txn
	Id     *common.TxnId
	vUUId  *common.VarUUId
	ballot *Ballot
	frame  *frame

	create *common.Positions
	read   *common.TxnId
	write  bool
	roll   bool

	addSub bool
	delSub *common.TxnId

	outcomeClock vc.VectorClock
	immigrantVar *msgs.Var
	writesClock  *vc.VectorClockImmutable
}

func (action *localAction) IsRead() bool {
	return action.read != nil
}

func (action *localAction) IsWrite() bool {
	return action.write || action.roll || action.addSub || action.delSub != nil
}

func (action *localAction) IsNoopWrite() bool {
	return !action.write && (action.roll || action.addSub || action.delSub != nil)
}

func (action *localAction) IsMeta() bool {
	return action.addSub || action.delSub != nil
}

func (action *localAction) IsImmigrant() bool {
	return action.immigrantVar != nil
}

func (action *localAction) VoteDeadlock(clock *vc.VectorClockMutable) {
	if action.ballot == nil {
		action.ballot = NewBallotBuilder(action.vUUId, AbortDeadlock, clock, nil).ToBallot()
		action.voteCast(action.ballot, true)
	}
}

func (action *localAction) VoteBadRead(clock *vc.VectorClockMutable, txnId *common.TxnId, actions *txnreader.TxnActions) {
	if action.ballot == nil {
		action.ballot = NewBallotBuilder(action.vUUId, AbortBadRead, clock, nil).CreateBadReadBallot(txnId, actions)
		action.voteCast(action.ballot, true)
	}
}

func (action *localAction) VoteCommit(clock *vc.VectorClockMutable, subscriptions *Subscriptions) bool {
	if action.ballot == nil {
		action.ballot = NewBallotBuilder(action.vUUId, Commit, clock, subscriptions).ToBallot()
		return !action.voteCast(action.ballot, false)
	}
	return false
}

// sl.Comparable interface
func (a *localAction) Compare(bC sl.Comparable) sl.Cmp {
	if bC == nil {
		if a == nil {
			return sl.EQ
		} else {
			return sl.GT
		}
	} else {
		b := bC.(*localAction)
		switch {
		case a == b:
			return sl.EQ
		case a == nil:
			return sl.LT
		case b == nil:
			return sl.GT
		default:
			return sl.Cmp(a.Id.Compare(b.Id))
		}
	}
}

func (action localAction) String() string {
	isCreate := action.create != nil
	isWrite := action.write
	f := ""
	if action.frame != nil {
		f = "|f"
	}
	b := ""
	if action.ballot != nil {
		b = "|b"
	}
	i := ""
	if action.IsImmigrant() {
		i = "|i"
	}
	s := ""
	if action.addSub {
		s = "|+"
	} else if action.delSub != nil {
		s = fmt.Sprintf("|-(%v)", action.delSub)
	}
	return fmt.Sprintf("Action from %v for %v: create:%v|read:%v|write:%v|roll:%v%s%s%s%s", action.Id, action.vUUId, isCreate, action.read, isWrite, action.roll, f, b, i, s)
}

func ImmigrationTxnFromCap(exe *dispatcher.Executor, vd *VarDispatcher, stateChange TxnLocalStateChange, reader *txnreader.TxnReader, varCaps msgs.Var_List, logger log.Logger) {
	txn := &Txn{
		logger:       logger,
		Id:           reader.Id,
		localActions: make([]localAction, varCaps.Len()),
		writes:       make(common.VarUUIds, 0, varCaps.Len()),
		TxnReader:    reader,
		exe:          exe,
		vd:           vd,
		stateChange:  stateChange,
	}
	// build localActions based only on the varCaps that we've received
	// - not on the actions of the txn, because the original txn action
	// allocations will have been based on the topology at the time,
	// and the topology has changed. So the emigrator will have figured
	// out which varcaps to send us based on what is now allocated to
	// this RM.
	actionsMap := make(map[common.VarUUId]*localAction)
	for idx, l := 0, varCaps.Len(); idx < l; idx++ {
		action := &txn.localActions[idx]
		action.Txn = txn
		varCap := varCaps.At(idx)
		action.Id = common.MakeTxnId(varCap.WriteTxnId())
		action.vUUId = common.MakeVarUUId(varCap.Id())
		positions := varCap.Positions()
		action.create = (*common.Positions)(&positions)
		action.write = true
		action.outcomeClock = vc.VectorClockFromData(varCap.WriteTxnClock(), false)
		action.immigrantVar = &varCap
		action.writesClock = vc.VectorClockFromData(varCap.WritesClock(), false)
		actionsMap[*action.vUUId] = action
		txn.writes = append(txn.writes, action.vUUId)
	}

	txn.Start(false) // txn will start at txnReceiveOutcome...
	txn.nextState()  // ...but that will get confused by outcomeClock
	// != nil, so we must take matters into our own
	// hands:
	enqueuedAt := time.Now()
	for idx := range txn.localActions {
		action := &txn.localActions[idx]
		f := func(v *Var) {
			if v == nil {
				panic(fmt.Sprintf("%v immigration error: %v unable to create var!", txn.Id, action.vUUId))
			} else {
				v.ReceiveTxnOutcome(action, enqueuedAt)
			}
		}
		vd.ApplyToVar(f, true, action.vUUId)
	}
}

func TxnFromReader(exe *dispatcher.Executor, vd *VarDispatcher, stateChange TxnLocalStateChange, ourRMId common.RMId, reader *txnreader.TxnReader, logger log.Logger) *Txn {
	txnId := reader.Id
	actions := reader.Actions(true)
	actionsList := actions.Actions()

	txn := &Txn{
		logger:      logger,
		Id:          txnId,
		writes:      make(common.VarUUIds, 0, actionsList.Len()),
		TxnReader:   reader,
		exe:         exe,
		vd:          vd,
		stateChange: stateChange,
	}

	allocations := reader.Txn.Allocations()
	for idx, l := 0, allocations.Len(); idx < l; idx++ {
		alloc := allocations.At(idx)
		rmId := common.RMId(alloc.RmId())
		if ourRMId == rmId {
			txn.populate(alloc.ActionIndices(), actionsList, actions)
			break
		}
	}

	return txn
}

func (txn *Txn) populate(actionIndices capn.UInt16List, actionsList *msgs.Action_List, actions *txnreader.TxnActions) {
	localActions := make([]localAction, actionIndices.Len())
	txn.localActions = localActions
	var action *localAction

	actionIndicesIdx := 0
	actionIndex := -1
	if actionIndicesIdx < actionIndices.Len() {
		actionIndex = int(actionIndices.At(actionIndicesIdx))
		action = &localActions[actionIndicesIdx]
	}

	for idx, l := 0, actionsList.Len(); idx < l; idx++ {
		action.Id = txn.Id
		actionCap := actionsList.At(idx)
		actionValue := actionCap.Value()

		isWrite := false

		if idx == actionIndex {
			action.Txn = txn
			action.vUUId = common.MakeVarUUId(actionCap.VarId())

			switch actionValue.Which() {
			case msgs.ACTIONVALUE_CREATE:
				actionCreate := actionValue.Create()
				positions := common.Positions(actionCreate.Positions())
				action.create = &positions
				isWrite = true

			case msgs.ACTIONVALUE_EXISTING:
				actionExisting := actionValue.Existing()
				if actionRead := actionExisting.Read(); len(actionRead) != 0 {
					action.read = common.MakeTxnId(actionRead)
				}
				switch actionModify := actionExisting.Modify(); actionModify.Which() {
				case msgs.ACTIONVALUEEXISTINGMODIFY_ROLL:
					action.roll = true
					isWrite = true
				case msgs.ACTIONVALUEEXISTINGMODIFY_WRITE:
					action.write = true
					isWrite = true
				}

			default:
				panic(fmt.Sprintf("Unexpected action value: %v", actionValue.Which()))
			}

			actionMeta := actionCap.Meta()
			if actionMeta.AddSub() {
				action.addSub = true
				isWrite = true
			}
			if delSub := actionMeta.DelSub(); len(delSub) != 0 {
				action.delSub = common.MakeTxnId(delSub)
				isWrite = true
			}

			actionIndicesIdx++
			if actionIndicesIdx < actionIndices.Len() {
				actionIndex = int(actionIndices.At(actionIndicesIdx))
				action = &localActions[actionIndicesIdx]
			}

		} else {
			isWrite = actionValue.Which() == msgs.ACTIONVALUE_CREATE
			if !isWrite && actionValue.Which() == msgs.ACTIONVALUE_EXISTING {
				modifyWhich := actionValue.Existing().Modify().Which()
				isWrite = modifyWhich != msgs.ACTIONVALUEEXISTINGMODIFY_NOT
			}
		}

		if isWrite {
			txn.writes = append(txn.writes, common.MakeVarUUId(actionCap.VarId()))
		}
	}
	if actionIndicesIdx != actionIndices.Len() {
		panic(fmt.Sprintf("Expected to find %v local actions, but only found %v", actionIndices.Len(), actionIndicesIdx))
	}
}

func (txn *Txn) Start(voter bool) {
	txn.voter = voter
	if voter {
		txn.txnDetermineLocalBallots.init(txn)
		txn.txnAwaitLocalBallots.init(txn)
	}
	txn.txnReceiveOutcome.init(txn)
	txn.txnAwaitLocallyComplete.init(txn)
	txn.txnReceiveCompletion.init(txn)

	if voter {
		txn.currentState = &txn.txnDetermineLocalBallots
	} else {
		txn.currentState = &txn.txnReceiveOutcome
	}
	txn.currentState.start()
}

func (txn *Txn) nextState() {
	switch txn.currentState {
	case &txn.txnDetermineLocalBallots:
		txn.currentState = &txn.txnAwaitLocalBallots
	case &txn.txnAwaitLocalBallots:
		txn.currentState = &txn.txnReceiveOutcome
	case &txn.txnReceiveOutcome:
		txn.currentState = &txn.txnAwaitLocallyComplete
	case &txn.txnAwaitLocallyComplete:
		txn.currentState = &txn.txnReceiveCompletion
	case &txn.txnReceiveCompletion:
		txn.currentState = nil
		return
	default:
		panic(fmt.Sprintf("%v Next state called on txn with txn in terminal state: %v\n", txn.Id, txn.currentState))
	}
	txn.currentState.start()
}

func (txn *Txn) String() string {
	return txn.Id.String()
}

func (txn *Txn) Status(sc *status.StatusConsumer) {
	sc.Emit(txn.Id.String())
	sc.Emit(fmt.Sprintf("- Local Actions: %v", txn.localActions))
	sc.Emit(fmt.Sprintf("- Current State: %v", txn.currentState))
	sc.Emit(fmt.Sprintf("- PreAborted? %v", txn.preAbortedBool))
	sc.Emit(fmt.Sprintf("- Aborted? %v", txn.aborted))
	sc.Emit(fmt.Sprintf("- Outcome Clock: %v", txn.outcomeClock))
	sc.Emit(fmt.Sprintf("- Active Frames Count: %v", atomic.LoadInt32(&txn.activeFramesCount)))
	sc.Emit(fmt.Sprintf("- Completed? %v", txn.completed))
	sc.Join()
}

// State machine

type txnStateMachineComponent interface {
	init(*Txn)
	start()
}

// Determine Local Ballots
type txnDetermineLocalBallots struct {
	*Txn
	pendingVote int32
}

func (tdb *txnDetermineLocalBallots) String() string { return "txnDetermineLocalBallots" }

func (tdb *txnDetermineLocalBallots) init(txn *Txn) {
	tdb.Txn = txn
	atomic.StoreInt32(&tdb.pendingVote, int32(len(tdb.localActions)))
}

func (tdb *txnDetermineLocalBallots) start() {
	tdb.nextState() // advance state FIRST!
	enqueuedAt := time.Now()
	for idx := 0; idx < len(tdb.localActions); idx++ {
		action := &tdb.localActions[idx]
		f := func(v *Var) {
			if v == nil {
				panic(fmt.Sprintf("%v error (%v): %v Unable to create var!", tdb.Id, tdb, action.vUUId))
			} else {
				v.ReceiveTxn(action, enqueuedAt)
			}
		}
		tdb.vd.ApplyToVar(f, true, action.vUUId)
	}
}

// Await Local Ballots
type txnAwaitLocalBallots struct {
	*Txn
	preAborted     int32
	preAbortedBool bool
}

func (talb *txnAwaitLocalBallots) String() string { return "txnAwaitLocalBallots" }

func (talb *txnAwaitLocalBallots) init(txn *Txn) {
	talb.Txn = txn
}

func (talb *txnAwaitLocalBallots) start() {}

func (talb *txnAwaitLocalBallots) voteCast(ballot *Ballot, abort bool) bool {
	if abort && atomic.CompareAndSwapInt32(&talb.preAborted, 0, 1) {
		talb.exe.EnqueueFuncAsync(talb.preAbort)
	}
	abort = abort || atomic.LoadInt32(&talb.preAborted) == 1
	if atomic.AddInt32(&talb.pendingVote, -1) == 0 {
		talb.exe.EnqueueFuncAsync(talb.allTxnBallotsComplete)
	}
	return abort
}

func (talb *txnAwaitLocalBallots) preAbort() (bool, error) {
	if talb.currentState == talb && !talb.preAbortedBool {
		talb.preAbortedBool = true
		for idx := 0; idx < len(talb.localActions); idx++ {
			action := &talb.localActions[idx]
			f := func(v *Var) {
				if action.ballot != nil && action.frame == nil {
					if v != nil { // no problem if v == nil - we've already voted to abort
						v.maybeMakeInactive()
					}
				} else if v == nil {
					panic(fmt.Sprintf("%v error (%v): %v not found!", talb.Id, talb, action.vUUId))
				} else if action.ballot != nil && action.frame != nil {
					if action.frame.v != v {
						panic(fmt.Sprintf("%v error (%v): %v has gone idle in the meantime somehow!", talb.Id, talb, action.vUUId))
					}
					switch {
					case action.IsRead() && action.IsWrite():
						action.frame.ReadWriteAborted(action, true)
					case action.IsRead():
						action.frame.ReadAborted(action)
					default:
						action.frame.WriteAborted(action, true)
					}
				}
			}
			talb.vd.ApplyToVar(f, false, action.vUUId)
		}
	} else {
		panic(fmt.Sprintf("%v error: preAbort with txn in wrong state (or preAbort called multiple times: %v): %v\n", talb.Id, talb.currentState, talb.preAbortedBool))
	}
	return false, nil
}

func (talb *txnAwaitLocalBallots) allTxnBallotsComplete() (bool, error) {
	if talb.currentState == talb {
		talb.nextState() // advance state FIRST!
		ballots := make([]*Ballot, len(talb.localActions))
		for idx := 0; idx < len(talb.localActions); idx++ {
			action := &talb.localActions[idx]
			ballots[idx] = action.ballot
		}
		talb.stateChange.TxnBallotsComplete(ballots...)
	} else {
		panic(fmt.Sprintf("%v error: Ballots completed with txn in wrong state: %v\n", talb.Id, talb.currentState))
	}
	return false, nil
}

// Receive Outcome
type txnReceiveOutcome struct {
	*Txn
	outcomeClock *vc.VectorClockImmutable
	aborted      bool
}

func (tro *txnReceiveOutcome) String() string { return "txnReceiveOutcome" }

func (tro *txnReceiveOutcome) init(txn *Txn) {
	tro.Txn = txn
}

func (tro *txnReceiveOutcome) start() {}

// Callback (from network/paxos)
func (tro *txnReceiveOutcome) BallotOutcomeReceived(outcome *msgs.Outcome) {
	if tro.outcomeClock != nil || tro.aborted {
		// We've already been here. Be silent if we receive extra outcomes.
		return
	}
	if tro.currentState != tro {
		// We've received the outcome too early! Be noisy!
		panic(fmt.Sprintf("%v error: Ballot outcome received with txn in wrong state: %v\n", tro.Id, tro.currentState))
	}
	switch outcome.Which() {
	case msgs.OUTCOME_COMMIT:
		tro.outcomeClock = vc.VectorClockFromData(outcome.Commit(), true)
		/*
			excess := tro.outcomeClock.Len - tro.TxnCap.Actions().Len()
			fmt.Printf("%v ", excess)
		*/
	default:
		tro.aborted = true
	}
	tro.nextState() // advance state FIRST!
	if tro.preAbortedBool {
		if !tro.aborted {
			panic(fmt.Sprintf("%v We preAborted the txn, but the txn outcome is to commit!", tro.Id))
		}
		return
	}
	for idx := 0; idx < len(tro.localActions); idx++ {
		action := &tro.localActions[idx]
		action.outcomeClock = tro.outcomeClock
		enqueuedAt := time.Now()
		f := func(v *Var) {
			if v == nil {
				panic(fmt.Sprintf("%v error (%v, aborted? %v, preAborted? %v, frame == nil? %v): %v not found!", tro.Id, tro, tro.aborted, tro.preAbortedBool, action.frame == nil, action.vUUId))
			} else {
				v.ReceiveTxnOutcome(action, enqueuedAt)
			}
		}
		// Should only have to create missing vars if we're a learner (i.e. !voter).
		tro.vd.ApplyToVar(f, !tro.voter, action.vUUId)
	}
}

// Await Locally Complete
type txnAwaitLocallyComplete struct {
	*Txn
	activeFramesCount int32
}

func (talc *txnAwaitLocallyComplete) String() string { return "txnAwaitLocallyComplete" }

func (talc *txnAwaitLocallyComplete) init(txn *Txn) {
	talc.Txn = txn
	atomic.StoreInt32(&talc.activeFramesCount, int32(len(talc.localActions)))
}

func (talc *txnAwaitLocallyComplete) start() {
	if talc.aborted || atomic.LoadInt32(&talc.activeFramesCount) == 0 {
		talc.locallyComplete()
	}
}

// Callback (from var-dispatcher (frames) back into txn)
func (talc *txnAwaitLocallyComplete) LocallyComplete() {
	result := atomic.AddInt32(&talc.activeFramesCount, -1)
	utils.DebugLog(talc.logger, "debug", "LocallyComplete", "TxnId", talc.Id, "pendingFrameCount", result)
	if result == 0 {
		talc.exe.EnqueueFuncAsync(talc.locallyComplete)
	} else if result < 0 {
		panic(fmt.Sprintf("%v activeFramesCount went -1!", talc.Id))
	}
}

func (talc *txnAwaitLocallyComplete) locallyComplete() (bool, error) {
	if talc.currentState == talc {
		talc.nextState() // do state first!
		talc.stateChange.TxnLocallyComplete(talc.Txn)
	}
	return false, nil
}

// Receive Completion
type txnReceiveCompletion struct {
	*Txn
	completed bool
}

func (trc *txnReceiveCompletion) String() string { return "txnReceiveCompletion" }

func (trc *txnReceiveCompletion) init(txn *Txn) {
	trc.Txn = txn
}

func (trc *txnReceiveCompletion) start() {}

// Callback (from network/paxos)
func (trc *txnReceiveCompletion) CompletionReceived() {
	utils.DebugLog(trc.logger, "debug", "CompletionReceived", "TxnId", trc.Id, "alreadyCompleted", trc.completed, "currentState", trc.currentState, "aborted", trc.aborted)
	if trc.completed {
		// Be silent in this case.
		return
	}
	if trc.currentState != trc {
		// We've been completed early! Be noisy!
		panic(fmt.Sprintf("%v error: Txn completion received with txn in wrong state: %v\n", trc.Id, trc.currentState))
	}
	trc.completed = true
	trc.maybeFinish()
	if trc.aborted {
		return
	}
	enqueuedAt := time.Now()
	for idx := 0; idx < len(trc.localActions); idx++ {
		action := &trc.localActions[idx]
		if action.frame == nil {
			// Could be the case if !aborted and we're a learner, but
			// when we learnt, we never assigned a frame.
			continue
		}
		f := func(v *Var) {
			if v == nil {
				panic(fmt.Sprintf("%v error (%v, aborted? %v, frame == nil? %v): %v Not found!", trc.Id, trc, trc.aborted, action.frame == nil, action.vUUId))
			} else {
				v.TxnGloballyComplete(action, enqueuedAt)
			}
		}
		trc.vd.ApplyToVar(f, false, action.vUUId)
	}
}

func (trc *txnReceiveCompletion) maybeFinish() {
	if trc.currentState == trc && trc.completed {
		trc.nextState()
		trc.stateChange.TxnFinished(trc.Txn)
	}
}
