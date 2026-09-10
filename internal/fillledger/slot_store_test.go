package fillledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func newSlotFixture(t testing.TB) (*SlotStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slots.db")
	s, err := CreateSlots(path, fixtureScope(), []SlotWrite{{Key: "100", State: []byte("empty")}, {Key: "101", State: []byte("untouched")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func slotTransaction(id int64) SlotTransaction {
	return SlotTransaction{Slots: []SlotWrite{{Key: "100", State: []byte("partial")}},
		Order: OrderCheckpoint{OrderID: id, ClientOrderID: "10000_B_1700000000001", Side: "BUY", SlotKey: "100", Status: "PARTIALLY_FILLED", ExecutedQty: "0.04", ExecutedQuote: "3.96", UpdateTime: 100, State: []byte("partial-progress")},
		Delta: Accounting{BuyQty: "0.04", SellQty: "0", RealizedPNL: "0"}}
}

func mustSlotRead(t testing.TB, s *SlotStore) SlotView {
	t.Helper()
	v, err := s.Read([]string{"100", "101"}, []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSlotTransactionPersistsOnlyAffectedRows(t *testing.T) {
	s, path := newSlotFixture(t)
	var untouched []byte
	if err := s.store.db.View(func(tx *bolt.Tx) error { untouched = bytes.Clone(tx.Bucket(slotBucket).Get([]byte("101"))); return nil }); err != nil {
		t.Fatal(err)
	}
	in := slotTransaction(1)
	result, err := s.Commit(in)
	if err != nil || result.Duplicate || result.View.Revision != 1 || len(result.View.Slots) != 1 || result.View.Totals.BuyQty != "0.04" || result.View.Totals.FilledOrders != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := s.store.db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(untouched, tx.Bucket(slotBucket).Get([]byte("101"))) {
			t.Fatal("unaffected row was rewritten")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in.Slots[0].State[0] = 'X'
	result.View.Slots["100"].State[0] = 'Y'
	result.View.Orders[1].State[0] = 'Z'
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSlots(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := mustSlotRead(t, s)
	if string(got.Slots["100"].State) != "partial" || string(got.Orders[1].State) != "partial-progress" || got.Slots["101"].Revision != 0 {
		t.Fatalf("recovery=%+v", got)
	}
	// An exact replay ignores the submitted old revision and returns current rows.
	second := slotTransaction(2)
	second.ExpectedRevision = 1
	second.Slots[0].State = []byte("new-order-state")
	if _, err := s.Commit(second); err != nil {
		t.Fatal(err)
	}
	replay := slotTransaction(1)
	replay.Order.ExecutedQty = "00.0400"
	replay.Order.ExecutedQuote = "03.960"
	replay.Order.ClientOrderID = "x-zdfVM8vY" + replay.Order.ClientOrderID
	dup, err := s.Commit(replay)
	if err != nil || !dup.Duplicate || dup.View.Revision != 2 || dup.View.Totals.BuyQty != "0.08" || string(dup.View.Slots["100"].State) != "new-order-state" {
		t.Fatalf("old replay=%+v err=%v", dup, err)
	}
}

func TestSlotTransactionsAcceptPartialTerminalAndPNLCorrection(t *testing.T) {
	s, path := newSlotFixture(t)
	in := slotTransaction(1)
	if _, err := s.Commit(in); err != nil {
		t.Fatal(err)
	}
	in.ExpectedRevision = 1
	in.Order.Status = "CANCELED"
	in.Order.ExecutedQty = "0.05"
	in.Order.ExecutedQuote = "5"
	in.Order.UpdateTime = 200
	in.Order.State = []byte("terminal")
	in.Delta.BuyQty = "0.01"
	in.Slots[0].State = []byte("terminal-slot")
	if _, err := s.Commit(in); err != nil {
		t.Fatal(err)
	}
	in.ExpectedRevision = 2
	in.Order.ExecutedQty = "0.06"
	in.Order.ExecutedQuote = "6.06"
	in.Order.UpdateTime = 300
	in.Order.State = []byte("late-terminal")
	if _, err := s.Commit(in); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSlots(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := mustSlotRead(t, s)
	if got.Revision != 3 || got.Totals.BuyQty != "0.06" || got.Totals.FilledOrders != 0 || got.Orders[1].Status != "CANCELED" {
		t.Fatalf("got=%+v", got)
	}
	// A sell can correct PNL at unchanged quantity, including a negative delta.
	sell := slotTransaction(2)
	sell.ExpectedRevision = 3
	sell.Order.Side = "SELL"
	sell.Order.Status = "CANCELED"
	sell.Delta = Accounting{BuyQty: "0", SellQty: "0.04", RealizedPNL: "0.11"}
	if _, err := s.Commit(sell); err != nil {
		t.Fatal(err)
	}
	sell.ExpectedRevision = 4
	sell.Order.UpdateTime = 200
	sell.Order.State = []byte("pnl-correction")
	sell.Delta.SellQty = "0"
	sell.Delta.RealizedPNL = "-0.02"
	r, err := s.Commit(sell)
	if err != nil || r.View.Totals.RealizedPNL != "0.09" || r.View.Totals.SellQty != "0.04" {
		t.Fatalf("pnl=%+v err=%v", r, err)
	}
}

func TestSlotRevisionConflictRequiresRecalculation(t *testing.T) {
	s, _ := newSlotFixture(t)
	if _, err := s.Commit(slotTransaction(1)); err != nil {
		t.Fatal(err)
	}
	stale := slotTransaction(2)
	if result, err := s.Commit(stale); !errors.Is(err, ErrRevisionConflict) || result.View.Slots != nil {
		t.Fatalf("stale result=%+v err=%v", result, err)
	}
	if s.Health() != nil {
		t.Fatal("revision conflict poisoned store")
	}
	stale.ExpectedRevision = 1
	if _, err := s.Commit(stale); err != nil {
		t.Fatal(err)
	}
	if got := mustSlotRead(t, s); got.Totals.BuyQty != "0.08" {
		t.Fatalf("lost increment: %+v", got)
	}
}

func TestSlotConcurrentDuplicateCommitsApplyOnce(t *testing.T) {
	s, _ := newSlotFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Commit(slotTransaction(1))
			if err != nil {
				t.Error(err)
				return
			}
			if !r.Duplicate {
				mu.Lock()
				applied++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if applied != 1 || mustSlotRead(t, s).Totals.BuyQty != "0.04" {
		t.Fatal("concurrent replay applied twice")
	}
}

func TestSlotConflictCannotOverwriteDurableProgress(t *testing.T) {
	for _, kind := range []string{"identity", "slot", "regressed-quantity", "regressed-quote", "same-quantity-new-quote", "wrong-delta", "filled-count", "filled-mutation", "terminal-reactivation"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newSlotFixture(t)
			first := slotTransaction(1)
			if kind == "filled-mutation" {
				first.Order.Status = "FILLED"
				first.Delta.FilledOrders = 1
			}
			if kind == "terminal-reactivation" {
				first.Order.Status = "CANCELED"
			}
			if _, err := s.Commit(first); err != nil {
				t.Fatal(err)
			}
			next := slotTransaction(1)
			next.ExpectedRevision = 1
			next.Order.UpdateTime = 200
			switch kind {
			case "identity":
				next.Order.ClientOrderID = "another-client"
			case "slot":
				next.Order.SlotKey = "101"
				next.Slots[0].Key = "101"
			case "regressed-quantity":
				next.Order.ExecutedQty = "0.03"
			case "regressed-quote":
				next.Order.ExecutedQty = "0.05"
				next.Order.ExecutedQuote = "3"
			case "same-quantity-new-quote":
				next.Order.ExecutedQuote = "4"
			case "wrong-delta":
				next.Order.ExecutedQty = "0.05"
				next.Order.ExecutedQuote = "5"
				next.Delta.BuyQty = "0.02"
			case "filled-count":
				next.Order.Status = "FILLED"
				next.Delta.BuyQty = "0"
			case "filled-mutation":
				next.Order.Status = "FILLED"
				next.Delta.FilledOrders = 1
			}
			r, err := s.Commit(next)
			if !errors.Is(err, ErrOrderConflict) || r.View.Slots != nil || !errors.Is(s.Health(), ErrUnavailable) {
				t.Fatalf("conflict result=%+v err=%v", r, err)
			}
		})
	}
}

func TestSlotTransactionFaultsAreAtomicAcrossRows(t *testing.T) {
	for _, mode := range []string{"before-write", "rollback-after-writes", "lost-commit-result"} {
		for _, fault := range []error{syscall.ENOSPC, io.ErrShortWrite, os.ErrPermission, syscall.EIO} {
			t.Run(mode+"/"+fault.Error(), func(t *testing.T) {
				s, path := newSlotFixture(t)
				in := slotTransaction(1)
				in.Slots = append(in.Slots, SlotWrite{Key: "101", State: []byte("child-updated")})
				s.store.db = &faultDatabase{database: s.store.db, mode: mode, err: fault}
				result, err := s.Commit(in)
				if !errors.Is(err, ErrUncertain) || !errors.Is(err, fault) || result.View.Slots != nil {
					t.Fatalf("uncertain result=%+v err=%v", result, err)
				}
				if _, err := s.Read([]string{"100"}, []int64{1}); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("read after fault=%v", err)
				}
				if _, err := s.Commit(in); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("write after fault=%v", err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				recovered, err := OpenSlots(path, fixtureScope())
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				before := mustSlotRead(t, recovered)
				committed := mode == "lost-commit-result"
				if (before.Revision == 1) != committed || (string(before.Slots["101"].State) == "child-updated") != committed || (len(before.Orders) == 1) != committed {
					t.Fatalf("torn state: %+v", before)
				}
				in.ExpectedRevision = before.Revision
				r, err := recovered.Commit(in)
				if err != nil || r.Duplicate != committed || r.View.Totals.BuyQty != "0.04" {
					t.Fatalf("replay=%+v err=%v", r, err)
				}
			})
		}
	}
}

func TestSlotReadFailureAndClosedStoreFailClosed(t *testing.T) {
	s, _ := newSlotFixture(t)
	s.store.db = &faultDatabase{database: s.store.db, mode: "read-failure", err: syscall.EIO}
	if v, err := s.Read(nil, []int64{123}); !errors.Is(err, syscall.EIO) || v.Orders != nil {
		t.Fatalf("missing vs error: %+v %v", v, err)
	}
	if _, err := s.Commit(slotTransaction(1)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestSlotRecoveryRejectsCorruptionAndOtherFormats(t *testing.T) {
	for _, kind := range []string{"slot-checksum", "order-checksum", "missing-slot", "missing-order", "head-count", "order-revision", "version", "scope"} {
		t.Run(kind, func(t *testing.T) {
			s, path := newSlotFixture(t)
			if _, err := s.Commit(slotTransaction(1)); err != nil {
				t.Fatal(err)
			}
			if err := s.store.db.Update(func(tx *bolt.Tx) error {
				slots, orders, meta := tx.Bucket(slotBucket), tx.Bucket(progressBucket), tx.Bucket(metaBucket)
				switch kind {
				case "slot-checksum":
					return slots.Put([]byte("100"), []byte("damaged"))
				case "order-checksum":
					return orders.Put(orderKey(1), []byte("damaged"))
				case "missing-slot":
					return slots.Delete([]byte("101"))
				case "missing-order":
					return orders.Delete(orderKey(1))
				case "head-count":
					h, _ := readSlotHead(tx)
					h.Totals.FilledOrders = 1
					return putSealed(meta, slotHeadKey, h)
				case "order-revision":
					r, _ := readOrderRecord(orderKey(1), orders.Get(orderKey(1)), 1)
					r.Revision = 2
					return putSealed(orders, orderKey(1), r)
				case "version":
					return putSealed(meta, headerKey, header{Version: 99, Scope: fixtureScope()})
				case "scope":
					h := fixtureScope()
					h.AccountRef = "wrong-account"
					return putSealed(meta, headerKey, header{Version: slotFormatVersion, Scope: h})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			got, err := OpenSlots(path, fixtureScope())
			if got != nil {
				got.Close()
				t.Fatal("invalid database opened")
			}
			want := ErrCorrupt
			if kind == "version" {
				want = ErrVersion
			}
			if kind == "scope" {
				want = ErrScopeMismatch
			}
			if !errors.Is(err, want) {
				t.Fatalf("err=%v want=%v", err, want)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("recovery changed evidence")
			}
		})
	}
	v1, path := newFixture(t)
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := OpenSlots(path, fixtureScope()); !errors.Is(err, ErrVersion) {
		if got != nil {
			got.Close()
		}
		t.Fatalf("v1 opened as v2: %v", err)
	}
	v2, path := newSlotFixture(t)
	if err := v2.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := Open(path, fixtureScope()); err == nil {
		got.Close()
		t.Fatal("v2 opened as v1")
	}
}

func TestSlotInvalidInputDoesNotWrite(t *testing.T) {
	for _, kind := range []string{"nan", "infinity", "negative", "oversize", "duplicate-slot", "missing-target", "missing-state", "fraction", "exponent"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newSlotFixture(t)
			in := slotTransaction(1)
			switch kind {
			case "nan":
				in.Delta.RealizedPNL = "NaN"
			case "infinity":
				in.Order.ExecutedQuote = "Inf"
			case "negative":
				in.Delta.BuyQty = "-1"
			case "oversize":
				in.Slots[0].State = make([]byte, MaxSlotStateBytes+1)
			case "duplicate-slot":
				in.Slots = append(in.Slots, SlotWrite{Key: "0100.0", State: []byte("duplicate")})
			case "missing-target":
				in.Slots[0].Key = "101"
			case "missing-state":
				in.Order.State = nil
			case "fraction":
				in.Order.ExecutedQty = "1/2"
			case "exponent":
				in.Order.ExecutedQty = "4e-2"
			}
			if _, err := s.Commit(in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err=%v", err)
			}
			if s.Health() != nil || mustSlotRead(t, s).Revision != 0 {
				t.Fatal("invalid input wrote/poisoned ledger")
			}
		})
	}
}

func TestSlotLedgerCrashChild(t *testing.T) {
	path := os.Getenv("OPENSQT_TEST_SLOT_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := OpenSlots(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	s.store.db = crashBeforeCommitDB{s.store.db}
	in := slotTransaction(1)
	in.Slots = append(in.Slots, SlotWrite{Key: "101", State: []byte("child")})
	if _, err := s.Commit(in); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child did not exit")
}

func TestSlotExitInsideTransactionRollsBackAllRows(t *testing.T) {
	s, path := newSlotFixture(t)
	before := mustSlotRead(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSlotLedgerCrashChild$")
	cmd.Env = append(os.Environ(), "OPENSQT_TEST_SLOT_CRASH_PATH="+path)
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("exit=%v output=%s", err, output)
	}
	s, err = OpenSlots(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := mustSlotRead(t, s); !reflect.DeepEqual(got, before) {
		t.Fatalf("partial transaction visible: %+v", got)
	}
}

func BenchmarkSlotTransaction(b *testing.B) {
	for _, size := range []int{1, 1000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			slots := make([]SlotWrite, size)
			for i := range slots {
				slots[i] = SlotWrite{Key: fmt.Sprint(100 + i), State: bytes.Repeat([]byte("s"), 1024)}
			}
			s, err := CreateSlots(filepath.Join(b.TempDir(), "bench.db"), fixtureScope(), slots)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			in := slotTransaction(1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				in.ExpectedRevision = uint64(i)
				in.Order.OrderID = int64(i + 1)
				if _, err := s.Commit(in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestSlotConcurrentDistinctOrdersRetryWithoutLosingTotals(t *testing.T) {
	const count = 16
	initial := make([]SlotWrite, count)
	for i := range initial {
		initial[i] = SlotWrite{Key: fmt.Sprint(100 + i), State: []byte("empty")}
	}
	s, err := CreateSlots(filepath.Join(t.TempDir(), "concurrent.db"), fixtureScope(), initial)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			in := slotTransaction(int64(i + 1))
			in.Slots[0].Key = initial[i].Key
			in.Order.SlotKey = initial[i].Key
			in.Order.Status = "FILLED"
			in.Delta.FilledOrders = 1
			for {
				view, err := s.Read([]string{initial[i].Key}, nil)
				if err != nil {
					t.Error(err)
					return
				}
				in.ExpectedRevision = view.Revision
				_, err = s.Commit(in)
				if errors.Is(err, ErrRevisionConflict) {
					continue
				}
				if err != nil {
					t.Error(err)
				}
				return
			}
		}()
	}
	close(start)
	wg.Wait()
	view, err := s.Read(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.Totals.BuyQty != "0.64" || view.Totals.FilledOrders != count || view.Revision != count {
		t.Fatalf("lost concurrent increments: %+v", view)
	}
}

func TestSlotAmountsPreservePrecisionBeyondFloat64(t *testing.T) {
	s, _ := newSlotFixture(t)
	qty := "0.100000000000000000000000000001"
	in := slotTransaction(1)
	in.Order.ExecutedQty = qty
	in.Delta.BuyQty = qty
	r, err := s.Commit(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.View.Orders[1].ExecutedQty != qty || r.View.Totals.BuyQty != qty {
		t.Fatal("decimal evidence rounded")
	}
	in.Order.OrderID = 2
	in.ExpectedRevision = 1
	r, err = s.Commit(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.View.Totals.BuyQty != "0.200000000000000000000000000002" {
		t.Fatalf("inexact totals: %s", r.View.Totals.BuyQty)
	}
}

func TestTerminalPNLCorrectionRequiresEventVersion(t *testing.T) {
	s, _ := newSlotFixture(t)
	in := slotTransaction(1)
	in.Order.Side = "SELL"
	in.Order.Status = "CANCELED"
	in.Delta = Accounting{BuyQty: "0", SellQty: "0.04", RealizedPNL: "0.1"}
	if _, err := s.Commit(in); err != nil {
		t.Fatal(err)
	}
	in.ExpectedRevision = 1
	in.Order.State = []byte("unversioned correction")
	in.Delta.SellQty = "0"
	in.Delta.RealizedPNL = "0.2"
	if _, err := s.Commit(in); !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("accepted unversioned PNL: %v", err)
	}
}
