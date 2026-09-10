package position

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"opensqt/internal/fillledger"
)

// This entire adapter is test-only. It exercises real reducer/publication code
// against a syncing ledger, without adding a persistence path to live trading.
// The DTO covers callback state, not the full startup/reservation/gap schema.
type replayOrderState struct {
	TerminalSeen bool
	Terminal     terminalOrderProgress
	Fill         *FilledOrderRecord
}

type replayEvent struct {
	Update     OrderUpdate
	Qty, Quote string // Literal exchange evidence, supplied BEFORE float conversion.
}

type replayStore interface {
	Read([]string, []int64) (fillledger.SlotView, error)
	Commit(fillledger.SlotTransaction) (fillledger.SlotCommitResult, error)
}

type ledgerReplay struct {
	spm     *SuperPositionManager
	ledger  replayStore
	failed  error
	notices int
	hook    func(string)
}

func replayScope() fillledger.Scope {
	return fillledger.Scope{Exchange: "binance", Environment: "testnet", AccountRef: "offline-replay-fixture", Symbol: "ETHUSDT", StrategyRevision: "order-callback-dto-v1"}
}

func replayJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
func replayFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return f
}
func replayDecimal(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
func replayQtyDelta(a, b string) string {
	x, ok := new(big.Rat).SetString(a)
	if !ok {
		panic(a)
	}
	y, ok := new(big.Rat).SetString(b)
	if !ok {
		panic(b)
	}
	scale := func(s string) int {
		if i := strings.IndexByte(s, '.'); i >= 0 {
			return len(s) - i - 1
		}
		return 0
	}
	value := x.Sub(x, y).FloatString(max(scale(a), scale(b)))
	if strings.Contains(value, ".") {
		value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}
	return value
}

func replayInitialSlot() orderSlotState {
	return orderSlotState{Price: 100, PositionStatus: PositionStatusEmpty, OrderStatus: OrderStatusNotPlaced, SlotStatus: SlotStatusFree}
}

func newReplayLedger(t testing.TB) (*fillledger.SlotStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay.db")
	s, err := fillledger.CreateSlots(path, replayScope(), []fillledger.SlotWrite{{Key: "100", State: replayJSON(replayInitialSlot())}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func restoreReplay(t testing.TB, s replayStore) *ledgerReplay {
	t.Helper()
	view, err := s.Read([]string{"100"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &ledgerReplay{spm: NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3), ledger: s}
	var state orderSlotState
	if err := json.Unmarshal(view.Slots["100"].State, &state); err != nil {
		t.Fatal(err)
	}
	slot := model.spm.lockMappedSlot(100)
	applyOrderSlotState(slot, state)
	slot.mu.Unlock()
	model.spm.totalBuyQty.Store(replayFloat(view.Totals.BuyQty))
	model.spm.totalSellQty.Store(replayFloat(view.Totals.SellQty))
	model.spm.realizedPNL.Store(replayFloat(view.Totals.RealizedPNL))
	model.spm.filledOrderCount = int64(view.Totals.FilledOrders)
	model.spm.SetAdjustmentNotifier(func(time.Duration) {
		// Real callback publication and notification must retain their lock boundary.
		if !slot.mu.TryLock() {
			panic("notification fired under slot lock")
		}
		slot.mu.Unlock()
		model.notices++
	})
	return model
}

func (r *ledgerReplay) process(event replayEvent) error {
	if r.failed != nil {
		return r.failed
	}
	var next orderTransition
	published := false
	err := func() error {
		update := event.Update
		update.ClientOrderID = r.spm.canonicalClientOrderID(update.ClientOrderID)
		price, side, valid := r.spm.parseClientOrderID(update.ClientOrderID)
		if !valid || price != 100 {
			return fillledger.ErrInvalid
		}
		slot := r.spm.lockMappedSlot(price)
		defer slot.mu.Unlock()
		view, err := r.ledger.Read([]string{"100"}, []int64{update.OrderID})
		if err != nil {
			return err
		}
		var before orderSlotState
		if err := json.Unmarshal(view.Slots["100"].State, &before); err != nil {
			return err
		}
		if !bytes.Equal(replayJSON(readOrderSlotState(slot)), replayJSON(before)) {
			return errors.New("live state must be restored before recalculation")
		}
		progress := replayOrderState{}
		previous, seen := view.Orders[update.OrderID]
		if seen {
			if previous.ClientOrderID != update.ClientOrderID || previous.Side != side || previous.SlotKey != "100" {
				return fillledger.ErrOrderConflict
			}
			if err := json.Unmarshal(previous.State, &progress); err != nil {
				return err
			}
			// A conflicting full-fill receipt is not allowed to disappear as a replay.
			if previous.Status == OrderStatusFilled && update.Status == OrderStatusFilled && (replayQtyDelta(event.Qty, previous.ExecutedQty) != "0" || replayQtyDelta(event.Quote, previous.ExecutedQuote) != "0") {
				return fillledger.ErrCompletionConflict
			}
		}
		in := r.spm.orderUpdateFacts(update, side, price, time.Unix(1700000000, 0).UTC())
		in.CanonicalSlotClientOID = r.spm.canonicalClientOrderID(before.ClientOID)
		in.AlreadyFilled = seen && previous.Status == OrderStatusFilled
		in.TerminalSeen, in.Terminal = progress.TerminalSeen, progress.Terminal
		next = reduceOrderUpdate(before, in)
		if next.Disposition == transitionFilledReplay || next.Disposition == transitionTerminalReplay || next.Disposition == transitionWrongIdentity || next.Issue == executionDuplicate || next.Issue == executionStaleQty && !next.StoreTerminal {
			return nil
		}
		if next.Issue != executionOK {
			return fmt.Errorf("fixture execution rejected: %s", next.Issue)
		}
		if bytes.Equal(replayJSON(before), replayJSON(next.Slot)) && next.Effect == (executionEffect{}) && !next.StoreTerminal && !next.RecordFill {
			return nil
		}
		if next.StoreTerminal {
			progress.TerminalSeen, progress.Terminal = true, next.Terminal
		}
		if next.RecordFill {
			fill := next.Fill
			progress.Fill = &fill
		}
		oldQty := "0"
		if seen {
			oldQty = previous.ExecutedQty
		}
		exactDelta := replayQtyDelta(event.Qty, oldQty)
		// Preserve exact fixture evidence in storage, and independently check the
		// legacy float reducer calculated the same quantity within its tolerance.
		if math.Abs(replayFloat(exactDelta)-next.Effect.BuyQty-next.Effect.SellQty) > fillQtyTolerance {
			return errors.New("fixture evidence disagrees with reducer")
		}
		delta := fillledger.ZeroAccounting()
		delta.RealizedPNL = replayDecimal(next.Effect.RealizedPNL)
		if side == "BUY" {
			delta.BuyQty = exactDelta
		} else {
			delta.SellQty = exactDelta
		}
		if next.RecordFill {
			delta.FilledOrders = 1
		}
		txn := fillledger.SlotTransaction{ExpectedRevision: view.Revision,
			Slots: []fillledger.SlotWrite{{Key: "100", State: replayJSON(next.Slot)}}, Delta: delta,
			Order: fillledger.OrderCheckpoint{OrderID: update.OrderID, ClientOrderID: update.ClientOrderID, Side: side, SlotKey: "100", Status: update.Status, ExecutedQty: event.Qty, ExecutedQuote: event.Quote, UpdateTime: max(update.UpdateTime, previous.UpdateTime), State: replayJSON(progress)}}
		if r.hook != nil {
			r.hook("before-commit")
		}
		result, err := r.ledger.Commit(txn)
		if err != nil {
			return err
		}
		if result.Duplicate {
			return errors.New("restore current returned view before publishing duplicate transaction")
		}
		if r.hook != nil {
			r.hook("after-durable-before-publish")
		}
		r.spm.publishOrderTransitionLocked(slot, in, next)
		published = true
		return nil
	}()
	if err != nil {
		r.failed = err
		return err
	}
	if published {
		if next.ReleaseStackCount > 1 {
			r.spm.releaseGapStackChildren(100, next.ReleaseStackCount)
		}
		if next.Adjust {
			r.spm.notifyAdjustment(0)
		}
		if r.hook != nil {
			r.hook("after-publish")
		}
	}
	return nil
}

func buyReplay(status, qty, quote string, millis int64) replayEvent {
	average := 0.0
	if qty != "0" {
		average = replayFloat(quote) / replayFloat(qty)
	}
	return replayEvent{Update: OrderUpdate{OrderID: 123, ClientOrderID: "10000_B_1700000000001", Side: "BUY", Status: status, Quantity: .5, ExecutedQty: replayFloat(qty), AvgPrice: average, Price: 100, UpdateTime: millis}, Qty: qty, Quote: quote}
}
func sellReplay(status, qty, quote, pnl string, millis int64) replayEvent {
	e := buyReplay(status, qty, quote, millis)
	e.Update.OrderID = 456
	e.Update.ClientOrderID = "10000_S_1700000000002"
	e.Update.Side = "SELL"
	e.Update.RealizedPNL = replayFloat(pnl)
	return e
}
func replaySnapshot(r *ledgerReplay) orderSlotState {
	slot := r.spm.lockMappedSlot(100)
	defer slot.mu.Unlock()
	return readOrderSlotState(slot)
}

func TestLedgerReplayUsesRealPartialCancelAndLateCorrection(t *testing.T) {
	s, path := newReplayLedger(t)
	r := restoreReplay(t, s)
	events := []replayEvent{buyReplay("PARTIALLY_FILLED", "0.125", "12.375", 100), buyReplay("CANCELED", "0.25", "25", 200)}
	for _, e := range events {
		if err := r.process(e); err != nil {
			t.Fatal(err)
		}
	}
	if r.notices != 1 {
		t.Fatalf("notices=%d", r.notices)
	}
	// Bind a new SELL before the old BUY's late terminal correction arrives.
	newSell := sellReplay("NEW", "0", "0", "0", 300)
	if err := r.process(newSell); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := fillledger.OpenSlots(path, replayScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r = restoreReplay(t, s)
	before := replaySnapshot(r)
	correction := buyReplay("CANCELED", "0.375", "37.875", 400)
	if err := r.process(correction); err != nil {
		t.Fatal(err)
	}
	after := replaySnapshot(r)
	assertClose(t, "late quantity", after.PositionQty, .375)
	assertClose(t, "late cost", after.PositionCost, 37.875)
	if after.OrderID != 456 || after.ClientOID != before.ClientOID || after.OrderStatus != before.OrderStatus || after.SlotStatus != before.SlotStatus || after.OrderFilledQty != before.OrderFilledQty {
		t.Fatal("old correction overwrote current binding")
	}
	if err := r.process(correction); err != nil {
		t.Fatal(err)
	}
	if err := r.process(events[1]); err != nil {
		t.Fatal(err)
	}
	assertClose(t, "old replay keeps latest quantity", replaySnapshot(r).PositionQty, .375)
	if r.notices != 1 {
		t.Fatalf("late correction notices=%d", r.notices)
	}
}

func TestLedgerReplayFullFillRestoresWithoutInMemoryDedup(t *testing.T) {
	s, path := newReplayLedger(t)
	r := restoreReplay(t, s)
	first := buyReplay("PARTIALLY_FILLED", "0.125", "12.375", 100)
	full := buyReplay("FILLED", "0.25", "25", 200)
	for _, e := range []replayEvent{first, full, full} {
		if err := r.process(e); err != nil {
			t.Fatal(err)
		}
	}
	if r.spm.filledOrderCount != 1 || r.notices != 1 {
		t.Fatalf("count/notices=%d/%d", r.spm.filledOrderCount, r.notices)
	}
	before := replaySnapshot(r)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := fillledger.OpenSlots(path, replayScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r = restoreReplay(t, s)
	if len(r.spm.filledOrderKeys) != 0 {
		t.Fatal("recovery loaded historical dedup keys")
	}
	full.Update.ClientOrderID = "x-zdfVM8vY" + full.Update.ClientOrderID
	for _, e := range []replayEvent{full, first, buyReplay("CANCELED", "0.25", "25", 300)} {
		if err := r.process(e); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(replayJSON(replaySnapshot(r)), replayJSON(before)) || r.spm.filledOrderCount != 1 || r.notices != 0 {
		t.Fatal("restart replay repeated inventory/accounting/notification")
	}
	assertClose(t, "recovered buys", r.spm.GetTotalBuyQty(), .25)
}

func TestLedgerReplaySellPNLCorrectionsMatchLiveCallback(t *testing.T) {
	s, path := newReplayLedger(t)
	r := restoreReplay(t, s)
	baseline := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	sequence := []replayEvent{
		buyReplay("FILLED", "0.5", "47.5", 100),
		sellReplay("PARTIALLY_FILLED", "0.125", "12.75", "0", 200),
		sellReplay("CANCELED", "0.25", "25.75", "0.6", 300),
		sellReplay("CANCELED", "0.375", "39", "0.7", 400),
		sellReplay("CANCELED", "0.375", "39", "0.65", 500),
	}
	for i, e := range sequence {
		if err := r.process(e); err != nil {
			t.Fatal(err)
		}
		baseline.OnOrderUpdate(e.Update)
		live := baseline.lockMappedSlot(100)
		want := readOrderSlotState(live)
		live.mu.Unlock()
		got := replaySnapshot(r)
		assertClose(t, "quantity", got.PositionQty, want.PositionQty)
		assertClose(t, "cost", got.PositionCost, want.PositionCost)
		assertClose(t, "account PNL", r.spm.GetRealizedPNL(), baseline.GetRealizedPNL())
		if i == 2 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			s, err = fillledger.OpenSlots(path, replayScope())
			if err != nil {
				t.Fatal(err)
			}
			r = restoreReplay(t, s)
		}
	}
	defer s.Close()
	old := sequence[3]
	if err := r.process(old); err != nil {
		t.Fatal(err)
	}
	if err := r.process(sequence[4]); err != nil {
		t.Fatal(err)
	}
	assertClose(t, "authoritative corrected PNL", r.spm.GetRealizedPNL(), .65)
	view, err := s.Read([]string{"100"}, []int64{456})
	if err != nil {
		t.Fatal(err)
	}
	var progress replayOrderState
	if err := json.Unmarshal(view.Orders[456].State, &progress); err != nil {
		t.Fatal(err)
	}
	assertClose(t, "persisted PNL high water", progress.Terminal.AccountedPNL, .65)
}

// A transaction boundary wrapper models rejected writes and a successful sync
// whose result is lost. Internal fillledger tests separately inject actual bbolt
// rollback after all row writes, plus ENOSPC/short-write/EIO failures.
type replayFailStore struct {
	replayStore
	after bool
}

func (s replayFailStore) Commit(tx fillledger.SlotTransaction) (fillledger.SlotCommitResult, error) {
	if s.after {
		if _, err := s.replayStore.Commit(tx); err != nil {
			return fillledger.SlotCommitResult{}, err
		}
	}
	return fillledger.SlotCommitResult{}, errors.Join(fillledger.ErrUncertain, errors.New("injected write/sync result failure"))
}

func TestLedgerFailureNeverPublishesRealStateOrNotifies(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			s, path := newReplayLedger(t)
			r := restoreReplay(t, s)
			before := replaySnapshot(r)
			r.ledger = replayFailStore{replayStore: s, after: after}
			fill := buyReplay("FILLED", "0.25", "25", 200)
			if err := r.process(fill); !errors.Is(err, fillledger.ErrUncertain) {
				t.Fatal(err)
			}
			if !bytes.Equal(replayJSON(replaySnapshot(r)), replayJSON(before)) || r.notices != 0 || r.spm.GetTotalBuyQty() != 0 || r.spm.filledOrderCount != 0 || len(r.spm.filledOrderKeys) != 0 || len(r.spm.terminalOrders) != 0 {
				t.Fatal("failed persistence leaked live state or notifications")
			}
			r.ledger = s
			if err := r.process(fill); err == nil {
				t.Fatal("failed adapter resumed without recovery")
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
			if (r.spm.GetTotalBuyQty() == .25) != after {
				t.Fatal("reopen chose wrong commit outcome")
			}
			if err := r.process(fill); err != nil {
				t.Fatal(err)
			}
			assertClose(t, "recovered quantity", replaySnapshot(r).PositionQty, .25)
			if r.spm.filledOrderCount != 1 {
				t.Fatal("replay double counted fill")
			}
			wantNotices := 1
			if after {
				wantNotices = 0
			}
			if r.notices != wantNotices {
				t.Fatalf("notices=%d", r.notices)
			}
		})
	}
}

func TestRealTransitionLedgerCrashChild(t *testing.T) {
	path := os.Getenv("OPENSQT_TEST_REPLAY_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := fillledger.OpenSlots(path, replayScope())
	if err != nil {
		t.Fatal(err)
	}
	r := restoreReplay(t, s)
	phase := os.Getenv("OPENSQT_TEST_REPLAY_CRASH_PHASE")
	r.hook = func(at string) {
		if at == phase {
			os.Exit(39)
		}
	}
	if err := r.process(buyReplay("FILLED", "0.25", "25", 200)); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child did not exit")
}

func TestRealTransitionCrashRecoveryAppliesExecutionOnce(t *testing.T) {
	for _, phase := range []string{"before-commit", "after-durable-before-publish", "after-publish"} {
		t.Run(phase, func(t *testing.T) {
			s, path := newReplayLedger(t)
			// Crash while completing an order whose partial fill is already durable.
			r := restoreReplay(t, s)
			if err := r.process(buyReplay("PARTIALLY_FILLED", "0.125", "12.375", 100)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRealTransitionLedgerCrashChild$")
			cmd.Env = append(os.Environ(), "OPENSQT_TEST_REPLAY_CRASH_PATH="+path, "OPENSQT_TEST_REPLAY_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 39 {
				t.Fatalf("exit=%v output=%s", err, output)
			}
			recovered, err := fillledger.OpenSlots(path, replayScope())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			r = restoreReplay(t, recovered)
			if phase == "before-commit" {
				assertClose(t, "uncommitted qty", replaySnapshot(r).PositionQty, .125)
				assertClose(t, "uncommitted cost", replaySnapshot(r).PositionCost, 12.375)
			} else {
				assertClose(t, "durable qty", replaySnapshot(r).PositionQty, .25)
				assertClose(t, "durable cost", replaySnapshot(r).PositionCost, 25)
			}
			full := buyReplay("FILLED", "0.25", "25", 200)
			if err := r.process(full); err != nil {
				t.Fatal(err)
			}
			if err := r.process(full); err != nil {
				t.Fatal(err)
			}
			assertClose(t, "final qty", replaySnapshot(r).PositionQty, .25)
			assertClose(t, "final cost", replaySnapshot(r).PositionCost, 25)
			if r.spm.filledOrderCount != 1 {
				t.Fatal("crash replay duplicated completion")
			}
		})
	}
}

func TestLedgerReplayCompletedSellPersistsCostAndPNLRecord(t *testing.T) {
	s, path := newReplayLedger(t)
	r := restoreReplay(t, s)
	events := []replayEvent{buyReplay("FILLED", "0.5", "47.5", 100), sellReplay("PARTIALLY_FILLED", "0.125", "12.75", "0", 200), sellReplay("FILLED", "0.5", "51.5", "0.8", 300)}
	for _, e := range events {
		if err := r.process(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := fillledger.OpenSlots(path, replayScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r = restoreReplay(t, s)
	if err := r.process(events[2]); err != nil {
		t.Fatal(err)
	}
	view, err := s.Read([]string{"100"}, []int64{456})
	if err != nil {
		t.Fatal(err)
	}
	var stored replayOrderState
	if err := json.Unmarshal(view.Orders[456].State, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Fill == nil {
		t.Fatal("completion record missing")
	}
	assertClose(t, "actual entry cost", stored.Fill.EntryPrice, 95)
	assertClose(t, "grid PNL", stored.Fill.GridPNL, 4)
	assertClose(t, "account PNL", stored.Fill.RealizedPNL, .8)
	assertClose(t, "restored account PNL", r.spm.GetRealizedPNL(), .8)
	if replaySnapshot(r).PositionCost != 0 || replaySnapshot(r).PositionQty != 0 || r.spm.filledOrderCount != 2 || r.notices != 0 {
		t.Fatal("sell replay repeated accounting")
	}
}

func TestLedgerFailedPNLCorrectionPreservesExistingAccounting(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			s, path := newReplayLedger(t)
			r := restoreReplay(t, s)
			for _, e := range []replayEvent{buyReplay("FILLED", "0.5", "47.5", 100), sellReplay("CANCELED", "0.25", "25.5", "0.6", 200)} {
				if err := r.process(e); err != nil {
					t.Fatal(err)
				}
			}
			before := replaySnapshot(r)
			notices := r.notices
			r.ledger = replayFailStore{replayStore: s, after: after}
			correction := sellReplay("CANCELED", "0.25", "25.5", "0.55", 300)
			if err := r.process(correction); !errors.Is(err, fillledger.ErrUncertain) {
				t.Fatal(err)
			}
			if !bytes.Equal(replayJSON(before), replayJSON(replaySnapshot(r))) || r.notices != notices {
				t.Fatal("failed correction changed live inventory or notifier")
			}
			assertClose(t, "unpublished PNL", r.spm.GetRealizedPNL(), .6)
			progress, found := r.spm.getTerminalOrderProgress(correction.Update)
			if !found {
				t.Fatal("lost existing terminal progress")
			}
			assertClose(t, "unpublished terminal correction", progress.AccountedPNL, .6)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := fillledger.OpenSlots(path, replayScope())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			r = restoreReplay(t, recovered)
			if err := r.process(correction); err != nil {
				t.Fatal(err)
			}
			assertClose(t, "recovered PNL", r.spm.GetRealizedPNL(), .55)
			assertClose(t, "unchanged cumulative sells", r.spm.GetTotalSellQty(), .25)
		})
	}
}
