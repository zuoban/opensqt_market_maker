package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/exchange"
	"opensqt/telemetry"
)

func TestTelemetrySubmissionBoundary(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(fmtTelemetryCase(native, failed), func(t *testing.T) {
				r := telemetry.New(time.Now())
				leased, callbacks := 0, 0
				checkBoundary := func(n int) {
					if leased != n || callbacks != n {
						t.Fatalf("lease/callback boundary = %d/%d want %d", leased, callbacks, n)
					}
				}
				ex := &batchRecordingExchange{batchSize: 5}
				ex.placeOrder = func(req *exchange.OrderRequest) (*exchange.Order, error) {
					checkBoundary(1)
					if failed {
						return nil, errors.New("simulated failure")
					}
					return &exchange.Order{OrderID: 1, Status: exchange.OrderStatusNew}, nil
				}
				ex.placeBatch = func(_ context.Context, reqs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
					checkBoundary(len(reqs))
					items := make([]exchange.PlaceOrderBatchItem, len(reqs))
					for i := range items {
						items[i].Order = &exchange.Order{OrderID: int64(i + 1), Status: exchange.OrderStatusNew}
					}
					if failed {
						items[0].Err = errors.New("simulated item failure")
						items[0].Order = nil
					}
					return items, nil
				}
				oe := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
				defer oe.Shutdown()
				oe.SetTelemetry(r)
				oe.rateLimiter = &countingWaiter{}
				makeRequest := func(id string) *OrderRequest {
					return &OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: .1, ClientOrderID: id,
						AcquireSubmissionLease: func() (func(), bool) { leased++; return func() { leased-- }, true },
						OnSubmissionStarted: func() {
							if leased == 0 {
								t.Fatal("callback outside lease")
							}
							callbacks++
						},
					}
				}
				var err error
				waits := uint64(1)
				if native {
					_, _, err = oe.BatchPlaceOrders([]*OrderRequest{makeRequest("a"), makeRequest("b")})
					waits = 2
				} else {
					_, err = oe.PlaceOrder(makeRequest("a"))
				}
				if (err != nil) != failed || leased != 0 {
					t.Fatalf("err=%v leases=%d", err, leased)
				}
				r.Refresh()
				s := r.Snapshot()
				wantErrors := uint64(0)
				if failed {
					wantErrors = 1
				}
				if s.Latencies[telemetry.PlaceRequest].Count != 1 || s.Latencies[telemetry.PlaceRequest].Errors != wantErrors ||
					s.Latencies[telemetry.RateLimitWait].Count != waits {
					t.Fatalf("incorrect request accounting: %+v", s.Latencies)
				}
			})
		}
	}
}

func fmtTelemetryCase(native, failed bool) string {
	name := "single"
	if native {
		name = "native"
	}
	if failed {
		name += "/failure"
	} else {
		name += "/success"
	}
	return name
}

func TestTelemetryLocalRejectionDoesNotStartSubmission(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, guard := range []string{"health", "maker", "lease", "limiter"} {
			t.Run(fmtTelemetryCase(native, false)+"/"+guard, func(t *testing.T) {
				ex := &batchRecordingExchange{}
				oe := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
				defer oe.Shutdown()
				r := telemetry.New(time.Now())
				oe.SetTelemetry(r)
				oe.rateLimiter = &countingWaiter{}
				req := &OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100.09, Quantity: .1, ClientOrderID: "a",
					OnSubmissionStarted: func() { t.Fatal("local rejection marked submitted") },
				}
				switch guard {
				case "health":
					oe.SetSubmissionHealthGuard(func() error { return errors.New("unhealthy") })
				case "maker":
					oe.SetMakerGuard(func() exchange.MarketSnapshot {
						return exchange.MarketSnapshot{Symbol: "ETHUSDT", BestBid: 100, BestAsk: 100.1, Ready: true, QuoteReceivedAt: time.Now(), QuoteVersion: 1, StreamEpoch: 1}
					}, .01, 2, time.Second)
				case "lease":
					req.AcquireSubmissionLease = func() (func(), bool) { return nil, false }
				case "limiter":
					oe.rateLimiter = &hookWaiter{before: func() { time.Sleep(2 * time.Millisecond); oe.StopNewOrders() }}
				}
				var err error
				if native {
					_, _, err = oe.BatchPlaceOrders([]*OrderRequest{req})
				} else {
					_, err = oe.PlaceOrder(req)
				}
				// Native batches silently discard stale reservations, as before.
				if err == nil && !(native && guard == "lease") {
					t.Fatal("expected local rejection")
				}
				r.Refresh()
				s := r.Snapshot()
				if len(ex.requests) != 0 || s.Latencies[telemetry.PlaceRequest].Count != 0 {
					t.Fatal("local failure counted as exchange call")
				}
				if guard == "limiter" {
					m := s.Latencies[telemetry.RateLimitWait]
					if m.Count != 1 || m.Errors != 1 || m.LastMS < 2 {
						t.Fatalf("wait canceled sample: %+v", m)
					}
				}
			})
		}
	}
}

func TestTelemetryCancelFallbackCountsActualCalls(t *testing.T) {
	ex := &cancelErrorExchange{batchErr: errors.New("batch failed"), singleErr: map[int64]error{2: errors.New("single failed")}}
	oe := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	defer oe.Shutdown()
	r := telemetry.New(time.Now())
	oe.SetTelemetry(r)
	oe.rateLimiter = &countingWaiter{}
	if err := oe.BatchCancelOrders([]int64{1, 2}); err == nil {
		t.Fatal("expected cancel failure")
	}
	r.Refresh()
	s := r.Snapshot()
	if s.Latencies[telemetry.CancelRequest].Count != 3 || s.Latencies[telemetry.CancelRequest].Errors != 2 || s.Latencies[telemetry.RateLimitWait].Count != 3 {
		t.Fatalf("cancel fallback accounting: %+v", s.Latencies)
	}
}
