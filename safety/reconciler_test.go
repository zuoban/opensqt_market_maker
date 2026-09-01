package safety

import (
	"context"
	"strings"
	"testing"
	"time"

	"opensqt/config"
)

type reconcileTestPosition struct {
	Symbol string
	Size   float64
}

type reconcileTestOrder struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Status        string
	Type          string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
}

type reconcileTestExchange struct {
	positions interface{}
	orders    interface{}
	posErr    error
	orderErr  error
}

func (e *reconcileTestExchange) GetPositions(context.Context, string) (interface{}, error) {
	return e.positions, e.posErr
}

func (e *reconcileTestExchange) GetOpenOrders(context.Context, string) (interface{}, error) {
	return e.orders, e.orderErr
}

func (e *reconcileTestExchange) GetBaseAsset() string { return "ETH" }

type reconcileTestSlot struct {
	PositionStatus string
	PositionQty    float64
	OrderID        int64
	ClientOID      string
	OrderSide      string
	OrderStatus    string
	OrderPrice     float64
	OrderQuantity  float64
	OrderFilledQty float64
	SlotStatus     string
}

type reconcileTestPM struct {
	slots          []reconcileTestSlot
	reconcileCount int64
}

func (p *reconcileTestPM) IterateSlots(fn func(float64, SlotInfo) bool) {
	for i, slot := range p.slots {
		if !fn(float64(i+1), SlotInfo{
			Price:          float64(i + 1),
			PositionStatus: slot.PositionStatus,
			PositionQty:    slot.PositionQty,
			OrderID:        slot.OrderID,
			ClientOID:      slot.ClientOID,
			OrderSide:      slot.OrderSide,
			OrderStatus:    slot.OrderStatus,
			OrderPrice:     slot.OrderPrice,
			OrderQuantity:  slot.OrderQuantity,
			OrderFilledQty: slot.OrderFilledQty,
			SlotStatus:     slot.SlotStatus,
		}) {
			return
		}
	}
}

func (p *reconcileTestPM) GetTotalBuyQty() float64           { return 0 }
func (p *reconcileTestPM) GetTotalSellQty() float64          { return 0 }
func (p *reconcileTestPM) GetReconcileCount() int64          { return p.reconcileCount }
func (p *reconcileTestPM) IncrementReconcileCount()          { p.reconcileCount++ }
func (p *reconcileTestPM) UpdateLastReconcileTime(time.Time) {}
func (p *reconcileTestPM) GetSymbol() string                 { return "ETHUSDT" }
func (p *reconcileTestPM) GetPriceInterval() float64         { return 1 }

func newTestReconciler(ex *reconcileTestExchange, pm *reconcileTestPM) *Reconciler {
	cfg := &config.Config{}
	cfg.Trading.Symbol = "ETHUSDT"
	return NewReconciler(cfg, ex, pm)
}

func TestReconcilerComparesRemoteState(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{
		{
			PositionStatus: "FILLED",
			PositionQty:    0.02,
			OrderID:        42,
			ClientOID:      "400000_S_1700000000001",
			OrderSide:      "SELL",
			OrderStatus:    "CONFIRMED",
			OrderPrice:     101,
			OrderQuantity:  0.02,
			SlotStatus:     "LOCKED",
		},
	}}
	ex := &reconcileTestExchange{
		positions: []*reconcileTestPosition{{Symbol: "ETHUSDT", Size: 0.02}},
		orders: []*reconcileTestOrder{{
			OrderID:       42,
			ClientOrderID: "x-zdfVM8vY400000_S_1700000000001",
			Symbol:        "ETHUSDT",
			Side:          "SELL",
			Status:        "NEW",
			Type:          "LIMIT",
			Price:         101,
			Quantity:      0.02,
		}},
	}
	r := newTestReconciler(ex, pm)
	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !r.IsHealthy() {
		t.Fatal("healthy = false, want true")
	}
	if pm.reconcileCount != 1 {
		t.Fatalf("reconcile count = %d, want 1", pm.reconcileCount)
	}
}

func TestReconcilerCountsPartiallyFilledBuyPosition(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{
		{
			PositionStatus: "EMPTY",
			PositionQty:    0.003,
			OrderID:        7,
			ClientOID:      "10000_B_1700000000001",
			OrderSide:      "BUY",
			OrderStatus:    "PARTIALLY_FILLED",
			OrderPrice:     100,
			OrderQuantity:  0.01,
			OrderFilledQty: 0.003,
			SlotStatus:     "LOCKED",
		},
	}}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{{Symbol: "ETHUSDT", Size: 0.003}},
		orders: []reconcileTestOrder{{
			OrderID:       7,
			ClientOrderID: "10000_B_1700000000001",
			Symbol:        "ETHUSDT",
			Side:          "BUY",
			Status:        "PARTIALLY_FILLED",
			Type:          "LIMIT",
			Price:         100,
			Quantity:      0.01,
			ExecutedQty:   0.003,
		}},
	}
	if err := newTestReconciler(ex, pm).Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcilerRejectsActiveOrderWhoseSlotInventoryPremiseWasInvalidated(t *testing.T) {
	tests := []struct {
		name       string
		slot       reconcileTestSlot
		remoteSize float64
		remote     reconcileTestOrder
		want       string
	}{
		{
			name: "BUY coexists with inventory from an old terminal correction",
			slot: reconcileTestSlot{
				PositionStatus: "FILLED", PositionQty: 0.003,
				OrderID: 31, ClientOID: "9900_B_1700000000001", OrderSide: "BUY",
				OrderStatus: "CONFIRMED", OrderPrice: 99, OrderQuantity: 0.01,
				SlotStatus: "LOCKED",
			},
			remoteSize: 0.003,
			remote: reconcileTestOrder{
				OrderID: 31, ClientOrderID: "9900_B_1700000000001", Symbol: "ETHUSDT",
				Side: "BUY", Status: "NEW", Type: "LIMIT", Price: 99, Quantity: 0.01,
			},
			want: "BUY 订单",
		},
		{
			name: "SELL quantity no longer covers the accepted order",
			slot: reconcileTestSlot{
				PositionStatus: "FILLED", PositionQty: 0.07,
				OrderID: 32, ClientOID: "10000_S_1700000000002", OrderSide: "SELL",
				OrderStatus: "CONFIRMED", OrderPrice: 101, OrderQuantity: 0.10,
				SlotStatus: "LOCKED",
			},
			remoteSize: 0.07,
			remote: reconcileTestOrder{
				OrderID: 32, ClientOrderID: "10000_S_1700000000002", Symbol: "ETHUSDT",
				Side: "SELL", Status: "NEW", Type: "LIMIT", Price: 101, Quantity: 0.10,
			},
			want: "SELL 订单",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := &reconcileTestPM{slots: []reconcileTestSlot{tt.slot}}
			ex := &reconcileTestExchange{
				positions: []reconcileTestPosition{{Symbol: "ETHUSDT", Size: tt.remoteSize}},
				orders:    []reconcileTestOrder{tt.remote},
			}
			r := newTestReconciler(ex, pm)
			err := r.Reconcile()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Reconcile() error = %v, want %q invariant failure", err, tt.want)
			}
			if r.IsHealthy() {
				t.Fatal("invalid slot/order coexistence was marked healthy")
			}
		})
	}
}

func TestReconcilerRequiresActiveOrdersToLockTheirSlots(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{{
		PositionStatus: "EMPTY",
		OrderID:        33, ClientOID: "9900_B_1700000000003", OrderSide: "BUY",
		OrderStatus: "CONFIRMED", OrderPrice: 99, OrderQuantity: 0.01,
		SlotStatus: "FREE",
	}}}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{},
		orders: []reconcileTestOrder{{
			OrderID: 33, ClientOrderID: "9900_B_1700000000003", Symbol: "ETHUSDT",
			Side: "BUY", Status: "NEW", Type: "LIMIT", Price: 99, Quantity: 0.01,
		}},
	}
	err := newTestReconciler(ex, pm).Reconcile()
	if err == nil || !strings.Contains(err.Error(), "LOCKED") {
		t.Fatalf("Reconcile() error = %v, want LOCKED invariant failure", err)
	}
}

func TestReconcilerRejectsPositionMismatchAndNotifies(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{{PositionStatus: "FILLED", PositionQty: 0.01}}}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{{Symbol: "ETHUSDT", Size: 0.02}},
		orders:    []reconcileTestOrder{},
	}
	r := newTestReconciler(ex, pm)
	var callbackErr error
	r.SetHealthHandler(func(healthy bool, err error) {
		if healthy {
			t.Error("unexpected healthy callback")
		}
		callbackErr = err
	})
	err := r.Reconcile()
	if err == nil || !strings.Contains(err.Error(), "持仓不一致") {
		t.Fatalf("Reconcile() error = %v, want position mismatch", err)
	}
	if r.IsHealthy() {
		t.Fatal("healthy = true, want false")
	}
	if callbackErr == nil {
		t.Fatal("health callback was not invoked")
	}
}

func TestReconcilerRejectsManagedOrphanButIgnoresManualOrder(t *testing.T) {
	pm := &reconcileTestPM{}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{},
		orders: []reconcileTestOrder{
			{OrderID: 100, ClientOrderID: "manual-order"},
			{
				OrderID:       101,
				ClientOrderID: "x-zdfVM8vY400000_B_1700000000001",
				Symbol:        "ETHUSDT",
				Side:          "BUY",
				Status:        "NEW",
				Type:          "LIMIT",
				Price:         4000,
				Quantity:      0.01,
			},
		},
	}
	err := newTestReconciler(ex, pm).Reconcile()
	if err == nil || !strings.Contains(err.Error(), "孤儿订单") {
		t.Fatalf("Reconcile() error = %v, want managed orphan", err)
	}

	ex.orders = []reconcileTestOrder{{OrderID: 100, ClientOrderID: "manual-order"}}
	if err := newTestReconciler(ex, pm).Reconcile(); err != nil {
		t.Fatalf("manual order should be ignored, got %v", err)
	}
}

func TestReconcilerRejectsEqualOppositeMissedPartialFills(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{
		{
			PositionStatus: "EMPTY",
			OrderID:        11,
			ClientOID:      "9900_B_1700000000001",
			OrderSide:      "BUY",
			OrderStatus:    "CONFIRMED",
			OrderPrice:     99,
			OrderQuantity:  0.01,
			SlotStatus:     "LOCKED",
		},
		{
			PositionStatus: "FILLED",
			PositionQty:    0.01,
			OrderID:        12,
			ClientOID:      "10000_S_1700000000002",
			OrderSide:      "SELL",
			OrderStatus:    "CONFIRMED",
			OrderPrice:     101,
			OrderQuantity:  0.01,
			SlotStatus:     "LOCKED",
		},
	}}
	ex := &reconcileTestExchange{
		// 等量买卖部分成交后净仓不变；只比较净仓和订单 ID 会错误放行。
		positions: []reconcileTestPosition{{Symbol: "ETHUSDT", Size: 0.01}},
		orders: []reconcileTestOrder{
			{
				OrderID: 11, ClientOrderID: "9900_B_1700000000001", Symbol: "ETHUSDT",
				Side: "BUY", Status: "PARTIALLY_FILLED", Type: "LIMIT",
				Price: 99, Quantity: 0.01, ExecutedQty: 0.002,
			},
			{
				OrderID: 12, ClientOrderID: "10000_S_1700000000002", Symbol: "ETHUSDT",
				Side: "SELL", Status: "PARTIALLY_FILLED", Type: "LIMIT",
				Price: 101, Quantity: 0.01, ExecutedQty: 0.002,
			},
		},
	}
	r := newTestReconciler(ex, pm)
	err := r.Reconcile()
	if err == nil || !strings.Contains(err.Error(), "ExecutedQty") {
		t.Fatalf("Reconcile() error = %v, want executedQty mismatch", err)
	}
	if r.IsHealthy() {
		t.Fatal("equal opposite missed fills were marked healthy")
	}
}

func TestReconcilerRejectsManagedOrderSideMismatch(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{{
		PositionStatus: "FILLED",
		PositionQty:    0.01,
		OrderID:        21,
		ClientOID:      "10000_S_1700000000001",
		OrderSide:      "SELL",
		OrderStatus:    "CONFIRMED",
		OrderPrice:     101,
		OrderQuantity:  0.01,
		SlotStatus:     "LOCKED",
	}}}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{{Symbol: "ETHUSDT", Size: 0.01}},
		orders: []reconcileTestOrder{{
			OrderID: 21, ClientOrderID: "10000_S_1700000000001", Symbol: "ETHUSDT",
			Side: "BUY", Status: "NEW", Type: "LIMIT", Price: 101, Quantity: 0.01,
		}},
	}
	err := newTestReconciler(ex, pm).Reconcile()
	if err == nil || !strings.Contains(err.Error(), "Side") {
		t.Fatalf("Reconcile() error = %v, want side mismatch", err)
	}
}

func TestReconcilerDoesNotTreatPendingSlotAsHealthy(t *testing.T) {
	pm := &reconcileTestPM{slots: []reconcileTestSlot{{SlotStatus: "PENDING"}}}
	ex := &reconcileTestExchange{
		positions: []reconcileTestPosition{},
		orders:    []reconcileTestOrder{},
	}
	r := newTestReconciler(ex, pm)
	err := r.Reconcile()
	if err == nil || !strings.Contains(err.Error(), "正在提交订单") {
		t.Fatalf("Reconcile() error = %v, want pending uncertainty", err)
	}
	if r.IsHealthy() {
		t.Fatal("pending slot was marked healthy")
	}
}
