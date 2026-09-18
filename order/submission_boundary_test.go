package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/exchange"
)

type boundaryTestExchange struct {
	exchange.IExchange
	call func(context.Context, *exchange.OrderRequest) (*exchange.Order, error)
}

func (*boundaryTestExchange) GetName() string { return "boundary-test" }
func (e *boundaryTestExchange) PlaceOrder(ctx context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
	return e.call(ctx, req)
}
func (*boundaryTestExchange) PlaceOrderBatchSize() int { return 5 }
func (e *boundaryTestExchange) PlaceOrderBatch(ctx context.Context, reqs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	items := make([]exchange.PlaceOrderBatchItem, len(reqs))
	for i, req := range reqs {
		items[i].Order, items[i].Err = e.call(ctx, req)
	}
	return items, nil
}

func TestSubmissionBoundaryReleasesConfirmationAndRechecksRetry(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, scenario := range []string{"success", "reservation", "health", "maker", "stopped", "reopened", "canceled", "limiter"} {
			t.Run(fmtTelemetryCase(native, false)+"/"+scenario, func(t *testing.T) {
				held, valid, healthy := false, true, true
				acquired, released, starts, posts, unknowns := 0, 0, 0, 0, 0
				market := exchange.MarketSnapshot{Symbol: "ETHUSDT", LastPrice: 100, BestBid: 99.9, BestAsk: 100.1,
					Ready: true, QuoteReceivedAt: time.Now(), ReceivedAt: time.Now(), QuoteVersion: 1, StreamEpoch: 1}
				ex := &boundaryTestExchange{}
				oe := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
				defer oe.Shutdown()
				waiter := &hookWaiter{}
				oe.rateLimiter = waiter
				oe.SetMakerGuard(func() exchange.MarketSnapshot { return market }, .01, 2, time.Second)
				oe.SetSubmissionHealthGuard(func() error {
					if !held {
						t.Error("health guard ran outside lease")
					}
					if !healthy {
						return errors.New("stream unhealthy")
					}
					return nil
				})
				req := &OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: .1, ClientOrderID: "same-id",
					AcquireSubmissionLease: func() (func(), bool) {
						if !valid {
							return nil, false
						}
						if held {
							t.Fatal("retry acquired an already-held lease")
						}
						held = true
						acquired++
						return func() {
							if !held {
								t.Error("lease released twice")
							}
							held = false
							released++
						}, true
					},
					OnSubmissionStarted: func() {
						if !held {
							t.Error("submission timer outside lease")
						}
						starts++
					},
					OnSubmissionUnknown: func() { unknowns++ },
				}
				ex.call = func(ctx context.Context, wire *exchange.OrderRequest) (*exchange.Order, error) {
					release, err := wire.BeginSubmission(ctx)
					if err != nil || !held {
						t.Fatalf("first boundary: held=%v err=%v", held, err)
					}
					posts++
					release()
					if held {
						t.Fatal("confirmation retained lease")
					}
					// 模拟 -2013 的只读确认期间，槽位、盘口或健康状态发生变化。
					switch scenario {
					case "reservation":
						valid = false
					case "health":
						healthy = false
					case "maker":
						market.BestAsk, market.BestBid, market.QuoteVersion = 98.1, 98, 2
					case "stopped":
						oe.StopNewOrders()
					case "reopened":
						oe.StopNewOrders()
						if err := oe.EnableNewOrders(); err != nil {
							t.Fatal(err)
						}
					case "canceled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					case "limiter":
						waiter.before = oe.StopNewOrders
					}
					retryRelease, err := wire.BeginSubmission(ctx)
					if err != nil {
						return nil, err
					}
					if scenario != "success" {
						t.Errorf("unsafe retry passed: %s", scenario)
					}
					if !held || !wire.PostOnly || wire.ClientOrderID != req.ClientOrderID {
						t.Fatal("retry lost lease, strict-maker flag or identity")
					}
					posts++
					retryRelease()
					return &exchange.Order{OrderID: 1, ClientOrderID: wire.ClientOrderID, Status: exchange.OrderStatusNew}, nil
				}
				var err error
				if native {
					_, _, err = oe.BatchPlaceOrders([]*OrderRequest{req})
				} else {
					_, err = oe.PlaceOrder(req)
				}
				if held || acquired != released {
					t.Fatalf("lease leak: held=%v acquired=%d released=%d", held, acquired, released)
				}
				if scenario == "success" {
					if err != nil || posts != 2 || starts != 2 || waiter.count != 2 || unknowns != 0 {
						t.Fatalf("retry: err=%v posts=%d starts=%d waits=%d unknown=%d", err, posts, starts, waiter.count, unknowns)
					}
				} else if !errors.Is(err, exchange.ErrOrderPlacementUnknown) || posts != 1 || unknowns == 0 {
					t.Fatalf("unsafe retry not retained as UNKNOWN: err=%v posts=%d unknown=%d", err, posts, unknowns)
				}
				wantEnabled := scenario == "success" || scenario == "reopened"
				if oe.newOrdersEnabled.Load() != wantEnabled {
					t.Fatalf("generation gate enabled=%v want=%v", oe.newOrdersEnabled.Load(), wantEnabled)
				}
			})
		}
	}
}
