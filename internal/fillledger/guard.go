package fillledger

import (
	"errors"
	"sync/atomic"
	"time"
)

var (
	ErrStorageDeadline = errors.New("ledger operation exceeded its deadline")
	ErrStorageBusy     = errors.New("ledger operation already in progress; retain event and retry in order")
)

type SlotAccess interface {
	Read([]string, []int64) (SlotView, error)
	Commit(SlotTransaction) (SlotCommitResult, error)
}

type guardState struct {
	deadline time.Time
	failure  error
}

// GuardedSlots is an offline integration prototype, not a production startup
// coordinator. Construct it only after opening/validating/restoring a ledger.
// Route all reads/commits through it. One operation is admitted at a time;
// ErrStorageBusy means the caller must retain the event, not drop or publish it.
// Health never takes the database lock. Failure is sticky with no reset method.
// Recovery must drain the old call, close/reopen, restore/reconcile, and create
// a new guard. A timeout does NOT cancel the underlying disk operation.
type GuardedSlots struct {
	store  SlotAccess
	limit  time.Duration
	state  atomic.Pointer[guardState]
	failed chan struct{}
}

func NewGuardedSlots(store SlotAccess, limit time.Duration) (*GuardedSlots, error) {
	if store == nil || limit <= 0 {
		return nil, ErrInvalid
	}
	return &GuardedSlots{store: store, limit: limit, failed: make(chan struct{})}, nil
}

// Failed closes once, also when the I/O call never returns. A coordinator may
// observe it outside business locks. No callback runs under a slot/DB lock here.
func (g *GuardedSlots) Failed() <-chan struct{} { return g.failed }

func (g *GuardedSlots) trip(active *guardState, cause error) {
	if g.state.CompareAndSwap(active, &guardState{failure: errors.Join(ErrUnavailable, cause)}) {
		close(g.failed)
	}
}

func (g *GuardedSlots) Health() error {
	for {
		s := g.state.Load()
		if s == nil {
			return nil
		}
		if s.failure != nil {
			return s.failure
		}
		if time.Now().Before(s.deadline) {
			return nil
		}
		// Do not rely on timer scheduling to enforce a submission-time check.
		g.trip(s, ErrStorageDeadline)
	}
}

func (g *GuardedSlots) begin() (*guardState, *time.Timer, error) {
	if err := g.Health(); err != nil {
		return nil, nil, err
	}
	s := &guardState{deadline: time.Now().Add(g.limit)}
	if !g.state.CompareAndSwap(nil, s) {
		if err := g.Health(); err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrStorageBusy
	}
	timer := time.AfterFunc(time.Until(s.deadline), func() { g.trip(s, ErrStorageDeadline) })
	return s, timer, nil
}

func (g *GuardedSlots) finish(s *guardState, timer *time.Timer, err error) error {
	timer.Stop()
	if !time.Now().Before(s.deadline) {
		g.trip(s, ErrStorageDeadline)
	}
	if err != nil && !guardInputError(err) {
		g.trip(s, err)
	}
	if g.state.CompareAndSwap(s, nil) {
		return err
	}
	return errors.Join(err, g.Health())
}

// Only a single wrapping chain ending in a validation/CAS error is recoverable.
// Joined errors may contain an I/O failure and must not be treated as healthy.
func guardInputError(err error) bool {
	for err != nil {
		if err == ErrInvalid || err == ErrRevisionConflict {
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func (g *GuardedSlots) Read(keys []string, ids []int64) (SlotView, error) {
	s, timer, err := g.begin()
	if err != nil {
		return SlotView{}, err
	}
	view, err := g.store.Read(keys, ids)
	if err = g.finish(s, timer, err); err != nil {
		return SlotView{}, err
	}
	return view, nil
}

func (g *GuardedSlots) Commit(in SlotTransaction) (SlotCommitResult, error) {
	s, timer, err := g.begin()
	if err != nil {
		return SlotCommitResult{}, err
	}
	result, err := g.store.Commit(in)
	if err = g.finish(s, timer, err); err != nil {
		if errors.Is(err, ErrUnavailable) {
			err = errors.Join(ErrUncertain, err)
		}
		return SlotCommitResult{}, err
	}
	return result, nil
}
