package sealing

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-statemachine"
	"github.com/filecoin-project/specs-actors/actors/abi"

	"github.com/filecoin-project/lotus/api"
)

func (m *Sealing) Plan(events []statemachine.Event, user interface{}) (interface{}, uint64, error) {
	next, err := m.plan(events, user.(*SectorInfo))
	if err != nil || next == nil {
		return nil, uint64(len(events)), err
	}

	return func(ctx statemachine.Context, si SectorInfo) error {
		err := next(ctx, si)
		if err != nil {
			log.Errorf("unhandled sector error (%d): %+v", si.SectorID, err)
			return nil
		}

		return nil
	}, uint64(len(events)), nil // TODO: This processed event count is not very correct
}

var fsmPlanners = map[api.SectorState]func(events []statemachine.Event, state *SectorInfo) error{
	api.UndefinedSectorState: planOne(on(SectorStart{}, api.Packing)),
	api.Packing:              planOne(on(SectorPacked{}, api.PreCommit1)),
	api.PreCommit1: planOne(
		on(SectorPreCommit1{}, api.PreCommit2),
		on(SectorSealPreCommitFailed{}, api.SealFailed),
		on(SectorPackingFailed{}, api.PackingFailed),
	),
	api.PreCommit2: planOne(
		on(SectorPreCommit2{}, api.PreCommitting),
		on(SectorSealPreCommitFailed{}, api.SealFailed),
		on(SectorPackingFailed{}, api.PackingFailed),
	),
	api.PreCommitting: planOne(
		on(SectorSealPreCommitFailed{}, api.SealFailed),
		on(SectorPreCommitted{}, api.WaitSeed),
		on(SectorChainPreCommitFailed{}, api.PreCommitFailed),
	),
	api.WaitSeed: planOne(
		on(SectorSeedReady{}, api.Committing),
		on(SectorChainPreCommitFailed{}, api.PreCommitFailed),
	),
	api.Committing: planCommitting,
	api.CommitWait: planOne(
		on(SectorProving{}, api.FinalizeSector),
		on(SectorCommitFailed{}, api.CommitFailed),
	),

	api.FinalizeSector: planOne(
		on(SectorFinalized{}, api.Proving),
	),

	api.Proving: planOne(
		on(SectorFaultReported{}, api.FaultReported),
		on(SectorFaulty{}, api.Faulty),
	),

	api.SealFailed: planOne(
		on(SectorRetrySeal{}, api.PreCommit1),
	),
	api.PreCommitFailed: planOne(
		on(SectorRetryPreCommit{}, api.PreCommitting),
		on(SectorRetryWaitSeed{}, api.WaitSeed),
		on(SectorSealPreCommitFailed{}, api.SealFailed),
	),
	api.ComputeProofFailed: planOne(
		on(SectorRetryComputeProof{}, api.Committing),
	),
	api.CommitFailed: planOne(
		on(SectorSealPreCommitFailed{}, api.SealFailed),
		on(SectorRetryWaitSeed{}, api.WaitSeed),
		on(SectorRetryComputeProof{}, api.Committing),
	),

	api.Faulty: planOne(
		on(SectorFaultReported{}, api.FaultReported),
	),
	api.FaultedFinal: final,
}

func (m *Sealing) plan(events []statemachine.Event, state *SectorInfo) (func(statemachine.Context, SectorInfo) error, error) {
	/////
	// First process all events

	for _, event := range events {
		e, err := json.Marshal(event)
		if err != nil {
			log.Errorf("marshaling event for logging: %+v", err)
			continue
		}

		l := Log{
			Timestamp: uint64(time.Now().Unix()),
			Message:   string(e),
			Kind:      fmt.Sprintf("event;%T", event.User),
		}

		if err, iserr := event.User.(xerrors.Formatter); iserr {
			l.Trace = fmt.Sprintf("%+v", err)
		}

		state.Log = append(state.Log, l)
	}

	p := fsmPlanners[state.State]
	if p == nil {
		return nil, xerrors.Errorf("planner for state %s not found", state.State)
	}

	if err := p(events, state); err != nil {
		return nil, xerrors.Errorf("running planner for state %s failed: %w", state.State, err)
	}

	/////
	// Now decide what to do next

	/*

		*   Empty
		|   |
		|   v
		*<- Packing <- incoming
		|   |
		|   v
		*<- PreCommit1 <--> SealFailed
		|   |                 ^^^
		|   v                 |||
		*<- PreCommit2 -------/||
		|   |                  ||
		|   v          /-------/|
		*   PreCommitting <-----+---> PreCommitFailed
		|   |                   |     ^
		|   v                   |     |
		*<- WaitSeed -----------+-----/
		|   |||  ^              |
		|   |||  \--------*-----/
		|   |||           |
		|   vvv      v----+----> ComputeProofFailed
		*<- Committing    |
		|   |        ^--> CommitFailed
		|   v             ^
		*<- CommitWait ---/
		|   |
		|   v
		*<- Proving
		|
		v
		FailedUnrecoverable

		UndefinedSectorState <- ¯\_(ツ)_/¯
		    |                     ^
		    *---------------------/

	*/

	switch state.State {
	// Happy path
	case api.Packing:
		return m.handlePacking, nil
	case api.PreCommit1:
		return m.handlePreCommit1, nil
	case api.PreCommit2:
		return m.handlePreCommit2, nil
	case api.PreCommitting:
		return m.handlePreCommitting, nil
	case api.WaitSeed:
		return m.handleWaitSeed, nil
	case api.Committing:
		return m.handleCommitting, nil
	case api.CommitWait:
		return m.handleCommitWait, nil
	case api.FinalizeSector:
		return m.handleFinalizeSector, nil
	case api.Proving:
		// TODO: track sector health / expiration
		log.Infof("Proving sector %d", state.SectorID)

	// Handled failure modes
	case api.SealFailed:
		return m.handleSealFailed, nil
	case api.PreCommitFailed:
		return m.handlePreCommitFailed, nil
	case api.ComputeProofFailed:
		return m.handleComputeProofFailed, nil
	case api.CommitFailed:
		return m.handleCommitFailed, nil

		// Faults
	case api.Faulty:
		return m.handleFaulty, nil
	case api.FaultReported:
		return m.handleFaultReported, nil

	// Fatal errors
	case api.UndefinedSectorState:
		log.Error("sector update with undefined state!")
	case api.FailedUnrecoverable:
		log.Errorf("sector %d failed unrecoverably", state.SectorID)
	default:
		log.Errorf("unexpected sector update state: %d", state.State)
	}

	return nil, nil
}

func planCommitting(events []statemachine.Event, state *SectorInfo) error {
	for _, event := range events {
		switch e := event.User.(type) {
		case globalMutator:
			if e.applyGlobal(state) {
				return nil
			}
		case SectorCommitted: // the normal case
			e.apply(state)
			state.State = api.CommitWait
		case SectorSeedReady: // seed changed :/
			if e.Seed.Equals(&state.Seed) {
				log.Warnf("planCommitting: got SectorSeedReady, but the seed didn't change")
				continue // or it didn't!
			}
			log.Warnf("planCommitting: commit Seed changed")
			e.apply(state)
			state.State = api.Committing
			return nil
		case SectorComputeProofFailed:
			state.State = api.ComputeProofFailed
		case SectorSealPreCommitFailed:
			state.State = api.CommitFailed
		case SectorCommitFailed:
			state.State = api.CommitFailed
		default:
			return xerrors.Errorf("planCommitting got event of unknown type %T, events: %+v", event.User, events)
		}
	}
	return nil
}

func (m *Sealing) restartSectors(ctx context.Context) error {
	trackedSectors, err := m.ListSectors()
	if err != nil {
		log.Errorf("loading sector list: %+v", err)
	}

	for _, sector := range trackedSectors {
		if err := m.sectors.Send(uint64(sector.SectorID), SectorRestart{}); err != nil {
			log.Errorf("restarting sector %d: %+v", sector.SectorID, err)
		}
	}

	// TODO: Grab on-chain sector set and diff with trackedSectors

	return nil
}

func (m *Sealing) ForceSectorState(ctx context.Context, id abi.SectorNumber, state api.SectorState) error {
	return m.sectors.Send(id, SectorForceState{state})
}

func final(events []statemachine.Event, state *SectorInfo) error {
	return xerrors.Errorf("didn't expect any events in state %s, got %+v", state.State, events)
}

func on(mut mutator, next api.SectorState) func() (mutator, api.SectorState) {
	return func() (mutator, api.SectorState) {
		return mut, next
	}
}

func planOne(ts ...func() (mut mutator, next api.SectorState)) func(events []statemachine.Event, state *SectorInfo) error {
	return func(events []statemachine.Event, state *SectorInfo) error {
		if len(events) != 1 {
			for _, event := range events {
				if gm, ok := event.User.(globalMutator); ok {
					gm.applyGlobal(state)
					return nil
				}
			}
			return xerrors.Errorf("planner for state %s only has a plan for a single event only, got %+v", state.State, events)
		}

		if gm, ok := events[0].User.(globalMutator); ok {
			gm.applyGlobal(state)
			return nil
		}

		for _, t := range ts {
			mut, next := t()

			if reflect.TypeOf(events[0].User) != reflect.TypeOf(mut) {
				continue
			}

			if err, iserr := events[0].User.(error); iserr {
				log.Warnf("sector %d got error event %T: %+v", state.SectorID, events[0].User, err)
			}

			events[0].User.(mutator).apply(state)
			state.State = next
			return nil
		}

		return xerrors.Errorf("planner for state %s received unexpected event %T (%+v)", state.State, events[0].User, events[0])
	}
}
