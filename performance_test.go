package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/order"
	"opensqt/position"
	"opensqt/telemetry"
)

type performanceBoundaryExchange struct {
	exchange.IExchange
	check  func(int)
	nextID int64
}

func (e *performanceBoundaryExchange) GetName() string { return "performance-test" }
func (e *performanceBoundaryExchange) PlaceOrder(_ context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
	e.check(1)
	return e.accept(req), nil
}
func (e *performanceBoundaryExchange) PlaceOrderBatchSize() int { return 5 }
func (e *performanceBoundaryExchange) PlaceOrderBatch(_ context.Context, reqs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	e.check(len(reqs))
	items := make([]exchange.PlaceOrderBatchItem, len(reqs))
	for i, req := range reqs {
		items[i].Order = e.accept(req)
	}
	return items, nil
}

func (e *performanceBoundaryExchange) accept(req *exchange.OrderRequest) *exchange.Order {
	e.nextID++
	return &exchange.Order{OrderID: e.nextID, ClientOrderID: req.ClientOrderID,
		Symbol: req.Symbol, Side: req.Side, Price: req.Price, Quantity: req.Quantity,
		Status: exchange.OrderStatusNew}
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

func TestAdapterForwardsPlacementBatchCapacity(t *testing.T) {
	native := &performanceBoundaryExchange{}
	// 只暴露基础接口的 wrapper 没有原生批量能力。
	sequential := struct{ exchange.IExchange }{native}
	for _, tc := range []struct {
		ex   exchange.IExchange
		want int
	}{{native, 5}, {sequential, 1}} {
		executor := order.NewExchangeOrderExecutor(tc.ex, "ETHUSDT", 0, 0)
		adapter := &exchangeExecutorAdapter{executor: executor}
		if got := adapter.PlacementBatchSize(); got != tc.want {
			t.Fatalf("planner batch limit=%d want=%d", got, tc.want)
		}
		executor.Shutdown()
	}
}

func TestOneBatchSchedulingThroughCoordinatorLoop(t *testing.T) {
	var submitted atomic.Int32
	finished := make(chan struct{}, 1)
	ex := &performanceBoundaryExchange{check: func(n int) {
		if n > 5 {
			t.Errorf("native batch overflow: %d", n)
		}
		if submitted.Add(int32(n)) == 19 {
			finished <- struct{}{}
		}
	}}
	executor := order.NewExchangeOrderExecutor(ex, "BTCUSDT", 0, 0)
	defer executor.Shutdown()
	cfg := &config.Config{}
	cfg.Trading.Symbol, cfg.Trading.PriceInterval, cfg.Trading.OrderQuantity = "BTCUSDT", 1, 30
	cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize, cfg.Trading.OrderCleanupThreshold = 19, 20, 100
	spm := position.NewSuperPositionManager(cfg, &exchangeExecutorAdapter{executor: executor}, nil, 2, 3)
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, spm)
	performance := telemetry.New(time.Now())
	runtime.performance.Store(performance)
	spm.SetTelemetry(performance)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatalf("same-price continuation stopped at %d orders", submitted.Load())
	}
	runtime.Stop() // 等待最后一批的本地绑定与观测写入完成。
	performance.Refresh()
	if submitted.Load() != 19 || performance.Snapshot().Latencies[telemetry.AdjustTotal].Count != 4 ||
		performance.Snapshot().Latencies[telemetry.AdjustRequestWait].Count != 3 {
		t.Fatalf("unexpected continuation counts: submitted=%d snapshot=%+v", submitted.Load(), performance.Snapshot())
	}
}
