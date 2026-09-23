package position

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"opensqt/exchange"
	orderpkg "opensqt/order"
	"opensqt/telemetry"
	"opensqt/utils"
)

// offlineBatchExecutor 模拟每批五笔的网络边界；虚拟时间只用于比较调度，
// 不替换生产时钟。lease 覆盖模拟请求，成交注入到另一个已挂买单槽位。
type offlineBatchExecutor struct {
	requests                     [][]*OrderRequest
	elapsed, rtt, fillAt, sellAt time.Duration
	injectFill                   func()
	beforeSubmit                 func([]*OrderRequest)
	err                          error
	nextID                       int64
}

func (e *offlineBatchExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }
func (e *offlineBatchExecutor) BatchCancelOrders([]int64) error          { return nil }
func (e *offlineBatchExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.requests = append(e.requests, append([]*OrderRequest(nil), requests...))
	var accepted []*Order
	for start := 0; start < len(requests); start += 5 {
		chunk := requests[start:min(start+5, len(requests))]
		var releases []func()
		for _, req := range chunk {
			release, ok := req.AcquireSubmissionLease()
			if !ok {
				panic("test submitted an invalid reservation")
			}
			releases = append(releases, release)
			if req.OnSubmissionStarted != nil {
				req.OnSubmissionStarted()
			}
			if req.Side == "SELL" && e.sellAt == 0 {
				e.sellAt = e.elapsed
			}
		}
		if e.beforeSubmit != nil {
			e.beforeSubmit(chunk)
		}
		e.elapsed += e.rtt / 2
		if e.injectFill != nil {
			inject := e.injectFill
			e.injectFill = nil
			e.fillAt = e.elapsed
			inject()
		}
		e.elapsed += e.rtt - e.rtt/2
		for _, release := range releases {
			release()
		}
		if e.err != nil {
			for _, req := range chunk {
				if errors.Is(e.err, exchange.ErrOrderPlacementUnknown) {
					req.MarkSubmissionUncertain()
				} else {
					req.MarkDefiniteRejection("post_only")
				}
			}
			return accepted, false, e.err
		}
		for _, req := range chunk {
			e.nextID++
			accepted = append(accepted, &Order{OrderID: e.nextID, ClientOrderID: req.ClientOrderID,
				Symbol: req.Symbol, Side: req.Side, Type: "LIMIT", Price: req.Price,
				Quantity: req.Quantity, Status: "NEW"})
		}
	}
	return accepted, false, nil
}

type limitedOfflineExecutor struct{ *offlineBatchExecutor }

func (*limitedOfflineExecutor) PlacementBatchSize() int { return 5 }

func batchTestManager(e OrderExecutorInterface) *SuperPositionManager {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 20, 20
	cfg.Trading.OrderCleanupThreshold = 100
	spm := NewSuperPositionManager(cfg, e, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	return spm
}

func TestAdjustmentYieldsAfterOneBatchWithoutReservingTail(t *testing.T) {
	e := &offlineBatchExecutor{}
	spm := batchTestManager(&limitedOfflineExecutor{e})
	wakes := 0
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		if delay <= 0 {
			wakes++
		}
		if !spm.adjustMu.TryLock() {
			t.Error("continuation notified while adjust lock held")
		} else {
			spm.adjustMu.Unlock()
		}
	})
	for round := 0; round < 4; round++ {
		if err := spm.AdjustOrders(100.2); err != nil {
			t.Fatal(err)
		}
		if len(e.requests[round]) != 5 {
			t.Fatalf("round %d submitted %d orders", round, len(e.requests[round]))
		}
		var active, pending, cooling int
		spm.forEachSlot(func(_ float64, slot *InventorySlot) bool {
			if slot.OrderID > 0 {
				active++
			}
			if slot.SlotStatus == SlotStatusPending {
				pending++
			}
			if !slot.placementRetryNotBefore.IsZero() {
				cooling++
			}
			return true
		})
		if active != (round+1)*5 || pending != 0 || cooling != 0 {
			t.Fatalf("round %d: active=%d pending=%d cooling=%d", round, active, pending, cooling)
		}
		if round < 3 && spm.ShouldSkipUnchangedGrid(100.2) {
			t.Fatal("remaining work lost to unchanged-grid skip")
		}
	}
	if wakes != 3 {
		t.Fatalf("continuation wakes=%d want=3", wakes)
	}
	if err := spm.AdjustOrders(100.2); err != nil {
		t.Fatal(err)
	}
	if len(e.requests) != 4 || wakes != 3 {
		t.Fatal("completed window kept submitting or waking")
	}
}

func TestBatchYieldRetainsUnknownAndDoesNotBypassRejectionCooldown(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			err := exchange.ErrOrderPlacementUnknown
			if !unknown {
				err = orderpkg.NewOrderRejectedError(orderpkg.OrderRejectionPostOnly, errors.New("post only"))
			}
			e := &offlineBatchExecutor{err: err}
			spm := batchTestManager(&limitedOfflineExecutor{e})
			var wakes []time.Duration
			spm.SetAdjustmentNotifier(func(d time.Duration) { wakes = append(wakes, d) })
			if got := spm.AdjustOrders(100.2); !errors.Is(got, err) {
				t.Fatalf("got=%v want=%v", got, err)
			}
			if len(e.requests) != 1 || len(e.requests[0]) != 5 {
				t.Fatal("submitted beyond first batch")
			}
			for _, req := range e.requests[0] {
				slot := spm.getOrCreateSlot(req.LogicalPrice)
				if unknown {
					if slot.ClientOID != req.ClientOrderID || slot.SlotStatus != SlotStatusPending {
						t.Fatal("unknown reservation released")
					}
				} else if slot.SlotStatus != SlotStatusFree || !slot.placementRetryNotBefore.After(time.Now()) {
					t.Fatal("rejection cooldown lost")
				}
			}
			if unknown && len(wakes) != 0 {
				t.Fatal("unknown batch scheduled continuation before recovery")
			}
			for _, d := range wakes {
				if d <= 0 {
					t.Fatal("batch yield bypassed cooldown")
				}
			}
			tail := spm.getOrCreateSlot(94)
			if tail.ClientOID != "" || tail.SlotStatus != SlotStatusFree {
				t.Fatal("unsent tail reserved")
			}
		})
	}
}

func TestBatchSchedulingFillLatencyWithSimulatedNetwork(t *testing.T) {
	for _, rtt := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second} {
		t.Run(rtt.String(), func(t *testing.T) {
			latencies := make([]time.Duration, 2)
			for mode := range 2 {
				e := &offlineBatchExecutor{rtt: rtt}
				var executor OrderExecutorInterface = e
				if mode == 1 {
					executor = &limitedOfflineExecutor{e}
				}
				spm := batchTestManager(executor)
				spm.SetTelemetry(telemetry.New(time.Now()))
				wakes := 0
				spm.SetAdjustmentNotifier(func(time.Duration) { wakes++ })
				slot := spm.getOrCreateSlot(99)
				oid := utils.GenerateOrderID(99, "BUY", 2)
				bindTerminalOrder(slot, 9999, oid, "BUY", .3)
				e.injectFill = func() {
					spm.OnOrderUpdate(OrderUpdate{OrderID: 9999, ClientOrderID: oid, Side: "BUY", Status: "FILLED", Quantity: .3, ExecutedQty: .3, AvgPrice: 99})
				}
				if err := spm.AdjustOrders(100.2); err != nil {
					t.Fatal(err)
				}
				if wakes == 0 || spm.TelemetryStateCounts().PendingOppositeSells != 1 {
					t.Fatal("fill did not leave observable pending sell and wake")
				}
				if err := spm.AdjustOrders(100.2); err != nil {
					t.Fatal(err)
				}
				if e.requests[1][0].Side != "SELL" || !e.requests[1][0].ReduceOnly || !e.requests[1][0].PostOnly {
					t.Fatal("opposite sell not prioritized safely")
				}
				if spm.TelemetryStateCounts().PendingOppositeSells != 0 {
					t.Fatal("accepted sell remained pending")
				}
				latencies[mode] = e.sellAt - e.fillAt
			}
			if latencies[0] != 7*rtt/2 || latencies[1] != rtt/2 {
				t.Fatalf("legacy/batched=%v want=%v/%v", latencies, 7*rtt/2, rtt/2)
			}
			t.Logf("simulated RTT=%s: legacy fill-to-sell=%s, one-batch=%s", rtt, latencies[0], latencies[1])
		})
	}
}
