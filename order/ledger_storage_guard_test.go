package order

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"opensqt/exchange"
	"opensqt/internal/fillledger"
)

// Only test code imports the ledger. All exchange calls remain local fakes.
type ledgerFaultAccess struct{ fillledger.SlotAccess }

func (s ledgerFaultAccess) Commit(fillledger.SlotTransaction) (fillledger.SlotCommitResult, error) {
	return fillledger.SlotCommitResult{}, errors.Join(fillledger.ErrUncertain, syscall.EIO)
}

func newLedgerGuardStore(t *testing.T) *fillledger.SlotStore {
	t.Helper()
	s, err := fillledger.CreateSlots(filepath.Join(t.TempDir(), "guard.db"), fillledger.Scope{
		Exchange: "binance", Environment: "testnet", AccountRef: "offline-guard", Symbol: "ETHUSDT", StrategyRevision: "guard-v1",
	}, []fillledger.SlotWrite{{Key: "100", State: []byte("empty")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newLedgerGate(t *testing.T) *fillledger.GuardedSlots {
	t.Helper()
	s := newLedgerGuardStore(t)
	g, err := fillledger.NewGuardedSlots(ledgerFaultAccess{s}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type ledgerStalledAccess struct {
	fillledger.SlotAccess
	entered, release chan struct{}
}

func (s ledgerStalledAccess) Commit(in fillledger.SlotTransaction) (fillledger.SlotCommitResult, error) {
	close(s.entered)
	<-s.release
	return s.SlotAccess.Commit(in)
}

func TestLedgerStorageDeadlineBlocksWhileCommitPending(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmtTelemetryCase(native, false), func(t *testing.T) {
			raw := newLedgerGuardStore(t)
			stalled := ledgerStalledAccess{raw, make(chan struct{}), make(chan struct{})}
			g, err := fillledger.NewGuardedSlots(stalled, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			defer release.Do(func() { close(stalled.release) })
			finished := make(chan error, 1)
			go func() { _, err := g.Commit(fillledger.SlotTransaction{}); finished <- err }()
			select {
			case <-stalled.entered:
			case <-time.After(time.Second):
				t.Fatal("fixture did not block")
			}
			select {
			case <-g.Failed():
			case <-time.After(time.Second):
				t.Fatal("deadline did not latch")
			}
			ex := &ledgerGuardExchange{}
			var adapter exchange.IExchange = ex
			if !native {
				adapter = ledgerSingleExchange{ex}
			}
			oe := NewExchangeOrderExecutor(adapter, "ETHUSDT", 0, 0)
			defer oe.Shutdown()
			oe.rateLimiter = &countingWaiter{}
			oe.SetSubmissionHealthGuard(g.Health)
			placed := make(chan error, 1)
			go func() {
				r := &OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: .1, ClientOrderID: "pending-storage"}
				if native {
					_, _, err := oe.BatchPlaceOrders([]*OrderRequest{r})
					placed <- err
				} else {
					_, err := oe.PlaceOrder(r)
					placed <- err
				}
			}()
			select {
			case err := <-placed:
				if !errors.Is(err, ErrTradingHealthGuardRejected) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("submission health waited for blocked storage")
			}
			if err := oe.CancelOrder(123); err != nil {
				t.Fatal(err)
			}
			if ex.posts != 0 || ex.batches != 0 || ex.cancels != 1 {
				t.Fatal("stalled storage boundary allowed placement or blocked cancel")
			}
			release.Do(func() { close(stalled.release) })
			if err := <-finished; !errors.Is(err, fillledger.ErrStorageDeadline) {
				t.Fatal(err)
			}
		})
	}
}

type ledgerGuardExchange struct {
	exchange.IExchange
	posts, batches, cancels int
	call                    func(context.Context, *exchange.OrderRequest) (*exchange.Order, error)
}

func (*ledgerGuardExchange) GetName() string { return "offline-ledger" }
func (e *ledgerGuardExchange) PlaceOrder(ctx context.Context, r *exchange.OrderRequest) (*exchange.Order, error) {
	e.posts++
	if e.call != nil {
		return e.call(ctx, r)
	}
	return &exchange.Order{OrderID: 1, ClientOrderID: r.ClientOrderID, Status: exchange.OrderStatusNew}, nil
}
func (*ledgerGuardExchange) PlaceOrderBatchSize() int { return 5 }
func (e *ledgerGuardExchange) PlaceOrderBatch(ctx context.Context, rs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	e.batches++
	items := make([]exchange.PlaceOrderBatchItem, len(rs))
	for i, r := range rs {
		items[i].Order, items[i].Err = e.PlaceOrder(ctx, r)
		if items[i].Err != nil {
			break
		}
	}
	return items, nil
}
func (e *ledgerGuardExchange) CancelOrder(ctx context.Context, _ string, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.cancels++
	return nil
}
func (e *ledgerGuardExchange) BatchCancelOrders(ctx context.Context, _ string, ids []int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.cancels += len(ids)
	return nil
}

type ledgerSingleExchange struct{ exchange.IExchange } // Hide optional native batching.

func TestLedgerStorageGuardBlocksEveryPlacementPath(t *testing.T) {
	for _, path := range []string{"single", "fallback-batch", "native-batch"} {
		for _, side := range []string{"BUY", "SELL"} {
			t.Run(path+"/"+side, func(t *testing.T) {
				g := newLedgerGate(t)
				ex := &ledgerGuardExchange{}
				var adapter exchange.IExchange = ex
				if path != "native-batch" {
					adapter = ledgerSingleExchange{ex}
				}
				oe := NewExchangeOrderExecutor(adapter, "ETHUSDT", 0, 0)
				defer oe.Shutdown()
				held, released, acquired, unknowns := 0, 0, 0, 0
				oe.SetSubmissionHealthGuard(func() error {
					if held == 0 {
						t.Error("health check outside submission lease")
					}
					return g.Health()
				})
				oe.rateLimiter = &hookWaiter{before: func() {
					// Storage fails after the caller observed health, before submission.
					if _, err := g.Commit(fillledger.SlotTransaction{}); !errors.Is(err, fillledger.ErrUnavailable) {
						t.Fatal(err)
					}
				}}
				request := func(id string) *OrderRequest {
					return &OrderRequest{
						Symbol: "ETHUSDT", Side: side, Price: 100, Quantity: .1, ReduceOnly: side == "SELL", ClientOrderID: id,
						AcquireSubmissionLease: func() (func(), bool) { held++; acquired++; return func() { held--; released++ }, true },
						OnSubmissionUnknown:    func() { unknowns++ },
					}
				}
				if g.Health() != nil {
					t.Fatal("fixture started unhealthy")
				}
				var err error
				if path == "single" {
					_, err = oe.PlaceOrder(request("kept-id"))
				} else {
					_, _, err = oe.BatchPlaceOrders([]*OrderRequest{request("kept-id"), request("second-id")})
				}
				if !errors.Is(err, ErrTradingHealthGuardRejected) || ex.posts != 0 || ex.batches != 0 || held != 0 || released != acquired || unknowns != 0 {
					t.Fatalf("unsafe boundary: err=%v calls=%d/%d lease=%d/%d held=%d unknown=%d", err, ex.posts, ex.batches, acquired, released, held, unknowns)
				}
				// Reconciliation/other health recovery cannot clear the storage latch.
				if err := oe.EnableNewOrders(); err != nil {
					t.Fatal(err)
				}
				if _, err := oe.PlaceOrder(request("after-enable")); !errors.Is(err, ErrTradingHealthGuardRejected) {
					t.Fatal(err)
				}
				if err := oe.CancelOrder(123); err != nil {
					t.Fatal(err)
				}
				if err := oe.BatchCancelOrders([]int64{456, 789}); err != nil {
					t.Fatal(err)
				}
				if ex.posts != 0 || ex.cancels != 3 {
					t.Fatal("storage failure blocked cancellation or allowed placement")
				}
			})
		}
	}
}

func TestLedgerStorageFailureDuringConfirmationKeepsUnknownIdentity(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmtTelemetryCase(native, false), func(t *testing.T) {
			g := newLedgerGate(t)
			held, unknown, creates := false, false, 0
			ex := &ledgerGuardExchange{}
			ex.call = func(ctx context.Context, wire *exchange.OrderRequest) (*exchange.Order, error) {
				release, err := wire.BeginSubmission(ctx)
				if err != nil {
					return nil, err
				}
				if !held || wire.ClientOrderID != "unchanged-unknown" || !wire.PostOnly {
					t.Fatal("first request lost identity/lease/maker")
				}
				creates++
				release()
				if _, err := g.Commit(fillledger.SlotTransaction{}); !errors.Is(err, fillledger.ErrUnavailable) {
					t.Fatal(err)
				}
				retryRelease, err := wire.BeginSubmission(ctx)
				if err == nil {
					retryRelease()
					t.Fatal("same-ID retry passed failed storage gate")
				}
				return nil, err
			}
			var adapter exchange.IExchange = ex
			if !native {
				adapter = ledgerSingleExchange{ex}
			}
			oe := NewExchangeOrderExecutor(adapter, "ETHUSDT", 0, 0)
			defer oe.Shutdown()
			oe.rateLimiter = &countingWaiter{}
			oe.SetSubmissionHealthGuard(g.Health)
			r := &OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: .1, ClientOrderID: "unchanged-unknown",
				AcquireSubmissionLease: func() (func(), bool) { held = true; return func() { held = false }, true },
				OnSubmissionUnknown:    func() { unknown = true },
				OnDefiniteRejection:    func(OrderRejectionKind) { t.Error("UNKNOWN converted into definite rejection") },
			}
			var err error
			if native {
				_, _, err = oe.BatchPlaceOrders([]*OrderRequest{r})
			} else {
				_, err = oe.PlaceOrder(r)
			}
			if !errors.Is(err, exchange.ErrOrderPlacementUnknown) || !unknown || creates != 1 || held || r.ClientOrderID != "unchanged-unknown" {
				t.Fatalf("lost UNKNOWN: %v", err)
			}
			if err := oe.CancelOrder(123); err != nil {
				t.Fatal(err)
			}
		})
	}
}
