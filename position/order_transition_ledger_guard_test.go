package position

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"opensqt/internal/fillledger"
)

type stalledReplayStore struct {
	replayStore
	entered, release chan struct{}
}

func (s stalledReplayStore) Commit(in fillledger.SlotTransaction) (fillledger.SlotCommitResult, error) {
	r, err := s.replayStore.Commit(in)
	close(s.entered)
	<-s.release
	return r, err
}

func TestLedgerStorageDeadlineNeverPublishesLateCommit(t *testing.T) {
	s, path := newReplayLedger(t)
	stalled := stalledReplayStore{s, make(chan struct{}), make(chan struct{})}
	g, err := fillledger.NewGuardedSlots(stalled, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := restoreReplay(t, g)
	before := replaySnapshot(r)
	var release sync.Once
	defer release.Do(func() { close(stalled.release) })
	fill := buyReplay("FILLED", "0.25", "25", 200)
	done := make(chan error, 1)
	go func() { done <- r.process(fill) }()
	select {
	case <-stalled.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("commit did not reach stall")
	}
	select {
	case <-g.Failed():
	case <-time.After(3 * time.Second):
		t.Fatal("storage deadline did not latch")
	}
	assertClose(t, "unpublished buy quantity", r.spm.GetTotalBuyQty(), 0)
	release.Do(func() { close(stalled.release) })
	if err := <-done; !errors.Is(err, fillledger.ErrUncertain) {
		t.Fatal(err)
	}
	if !bytes.Equal(replayJSON(before), replayJSON(replaySnapshot(r))) || r.notices != 0 || r.spm.filledOrderCount != 0 {
		t.Fatal("late commit leaked publication or notification")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := fillledger.OpenSlots(path, replayScope())
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	r = restoreReplay(t, recovered)
	if err := r.process(fill); err != nil {
		t.Fatal(err)
	}
	assertClose(t, "restored quantity", replaySnapshot(r).PositionQty, .25)
	if r.spm.filledOrderCount != 1 || r.notices != 0 {
		t.Fatal("recovery repeated the already durable fill")
	}
}
