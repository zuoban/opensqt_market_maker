package main

import (
	"context"
	"testing"

	"opensqt/exchange"
	"opensqt/order"
	"opensqt/position"
	"opensqt/telemetry"
)

type performanceBoundaryExchange struct {
	exchange.IExchange
	check func(int)
}

func (e *performanceBoundaryExchange) GetName() string { return "performance-test" }
func (e *performanceBoundaryExchange) PlaceOrder(_ context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
	e.check(1)
	return &exchange.Order{OrderID: 1, ClientOrderID: req.ClientOrderID, Status: exchange.OrderStatusNew}, nil
}
func (e *performanceBoundaryExchange) PlaceOrderBatchSize() int { return 5 }
func (e *performanceBoundaryExchange) PlaceOrderBatch(_ context.Context, reqs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	e.check(len(reqs))
	items := make([]exchange.PlaceOrderBatchItem, len(reqs))
	for i, req := range reqs {
		items[i].Order = &exchange.Order{OrderID: int64(i + 1), ClientOrderID: req.ClientOrderID, Status: exchange.OrderStatusNew}
	}
	return items, nil
}

func TestAdapterForwardsSubmissionTimingUnderLease(t *testing.T) {
	for _, batch := range []bool{false, true} {
		callbacks, leases := 0, 0
		ex := &performanceBoundaryExchange{check: func(n int) {
			if callbacks != n || leases != n {
				t.Fatalf("callback/leases=%d/%d, want %d", callbacks, leases, n)
			}
		}}
		executor := order.NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
		adapter := &exchangeExecutorAdapter{executor: executor}
		request := func(id string) *position.OrderRequest {
			return &position.OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: .1, ClientOrderID: id,
				AcquireSubmissionLease: func() (func(), bool) { leases++; return func() { leases-- }, true },
				OnSubmissionStarted:    func() { callbacks++ },
			}
		}
		var err error
		if batch {
			_, _, err = adapter.BatchPlaceOrders([]*position.OrderRequest{request("a"), request("b")})
		} else {
			_, err = adapter.PlaceOrder(request("a"))
		}
		executor.Shutdown()
		if err != nil || leases != 0 {
			t.Fatalf("err=%v remaining leases=%d", err, leases)
		}
	}
}

func TestPerformanceLogMissingSample(t *testing.T) {
	s := telemetry.Snapshot{}
	if performanceP95(s, telemetry.Planning) != "--(n=0)" {
		t.Fatal("missing sample presented as measured zero")
	}
	s.Latencies[telemetry.Planning] = telemetry.LatencySnapshot{Samples: 10, P95MS: 1.25}
	if performanceP95(s, telemetry.Planning) != "1.25ms(n=10)" {
		t.Fatal("log statistic missing units/count")
	}
}
