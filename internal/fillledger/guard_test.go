package fillledger

import (
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type stalledDatabase struct {
	database
	mode             string
	entered, release chan struct{}
}

func (d *stalledDatabase) stall() { close(d.entered); <-d.release }
func (d *stalledDatabase) View(fn func(*bolt.Tx) error) error {
	if d.mode == "read" {
		d.stall()
	}
	return d.database.View(fn)
}
func (d *stalledDatabase) Update(fn func(*bolt.Tx) error) error {
	if d.mode == "before-write" {
		d.stall()
	}
	err := d.database.Update(fn)
	if d.mode == "after-durable" {
		d.stall()
	}
	return err
}

func awaitGuardSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("guard signal did not arrive")
	}
}

func TestGuardedSlotsDeadlineDoesNotWaitForDatabaseLock(t *testing.T) {
	for _, mode := range []string{"read", "before-write", "after-durable"} {
		t.Run(mode, func(t *testing.T) {
			s, path := newSlotFixture(t)
			d := &stalledDatabase{database: s.store.db, mode: mode, entered: make(chan struct{}), release: make(chan struct{})}
			s.store.db = d
			g, err := NewGuardedSlots(s, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			defer release.Do(func() { close(d.release) })
			done := make(chan error, 1)
			go func() {
				if mode == "read" {
					view, err := g.Read([]string{"100"}, nil)
					if view.Slots != nil {
						done <- errors.New("timed-out read exposed state")
						return
					}
					done <- err
				} else {
					result, err := g.Commit(slotTransaction(1))
					if result.View.Slots != nil || result.Duplicate {
						done <- errors.New("timed-out commit exposed state")
						return
					}
					done <- err
				}
			}()
			awaitGuardSignal(t, d.entered)
			if s.store.mu.TryLock() {
				s.store.mu.Unlock()
				t.Fatal("fixture did not hold the real store mutex")
			}
			awaitGuardSignal(t, g.Failed()) // Independent timer, without calling Health.
			health := make(chan error, 1)
			go func() { health <- g.Health() }()
			select {
			case err := <-health:
				if !errors.Is(err, ErrStorageDeadline) || !errors.Is(err, ErrUnavailable) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("health waited on blocked disk I/O")
			}
			if _, err := g.Read(nil, nil); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			release.Do(func() { close(d.release) })
			select {
			case err := <-done:
				if !errors.Is(err, ErrStorageDeadline) || mode != "read" && !errors.Is(err, ErrUncertain) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("released I/O did not finish")
			}
			if g.Health() == nil {
				t.Fatal("late success cleared storage failure")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSlots(path, fixtureScope())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			view := mustSlotRead(t, reopened)
			want := uint64(1)
			if mode == "read" {
				want = 0
			}
			if view.Revision != want {
				t.Fatal("timeout was incorrectly treated as write cancellation")
			}
		})
	}
}

func TestGuardedSlotsFaultsLatch(t *testing.T) {
	for _, mode := range []string{"read-failure", "before-write", "rollback-after-writes", "lost-commit-result"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newSlotFixture(t)
			s.store.db = &faultDatabase{database: s.store.db, mode: mode, err: syscall.EIO}
			g, err := NewGuardedSlots(s, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "read-failure" {
				_, err = g.Read(nil, nil)
			} else {
				_, err = g.Commit(slotTransaction(1))
			}
			if !errors.Is(err, syscall.EIO) || !errors.Is(g.Health(), ErrUnavailable) {
				t.Fatal(err)
			}
			awaitGuardSignal(t, g.Failed())
			if _, err := g.Commit(slotTransaction(1)); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
		})
	}
}

func TestGuardedSlotsAdmissionAndStaleTimer(t *testing.T) {
	s, _ := newSlotFixture(t)
	g, err := NewGuardedSlots(s, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, timer, err := g.begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Read(nil, nil); !errors.Is(err, ErrStorageBusy) {
		t.Fatal(err)
	}
	if err := g.finish(first, timer, nil); err != nil {
		t.Fatal(err)
	}
	second, timer, err := g.begin()
	if err != nil {
		t.Fatal(err)
	}
	g.trip(first, ErrStorageDeadline) // A previously queued timer callback.
	if err := g.Health(); err != nil {
		t.Fatal(err)
	}
	if err := g.finish(second, timer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Commit(SlotTransaction{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	in := slotTransaction(1)
	in.ExpectedRevision = 9
	if _, err := g.Commit(in); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal(err)
	}
	if g.Health() != nil {
		t.Fatal("ordinary validation/CAS error latched storage failure")
	}
	if _, err := g.Commit(slotTransaction(1)); err != nil {
		t.Fatal(err)
	}
	// A submission-time check must enforce the deadline even before a timer runs.
	g.state.Store(&guardState{deadline: time.Now().Add(-time.Second)})
	if !errors.Is(g.Health(), ErrStorageDeadline) {
		t.Fatal("overdue operation passed health check")
	}
}

func TestGuardedSlotsCompletionTimeoutRace(t *testing.T) {
	for range 100 {
		g, _ := NewGuardedSlots(&SlotStore{}, time.Minute)
		s, timer, _ := g.begin()
		start, done := make(chan struct{}), make(chan error, 1)
		go func() { <-start; done <- g.finish(s, timer, nil) }()
		close(start)
		g.trip(s, ErrStorageDeadline)
		err := <-done
		if (err == nil) != (g.Health() == nil) {
			t.Fatal("late timer or completion resurrected failed state")
		}
	}
}

type joinedGuardFault struct{ SlotAccess }

func (s joinedGuardFault) Commit(SlotTransaction) (SlotCommitResult, error) {
	return SlotCommitResult{}, errors.Join(ErrRevisionConflict, syscall.EIO)
}

func TestGuardedSlotsJoinedErrorCannotHideIOFailure(t *testing.T) {
	s, _ := newSlotFixture(t)
	g, err := NewGuardedSlots(joinedGuardFault{s}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Commit(slotTransaction(1)); !errors.Is(err, syscall.EIO) || !errors.Is(g.Health(), ErrUnavailable) {
		t.Fatalf("I/O fault mistaken for recoverable revision conflict: %v", err)
	}
}
