package safety

import (
	"reflect"
	"sync"
	"testing"

	"opensqt/config"
)

type orderCleanerTestSlot struct {
	OrderID     int64
	ClientOID   string
	OrderSide   string
	OrderStatus string
}

type orderCleanerTestPM struct {
	mu         sync.Mutex
	slots      map[float64]orderCleanerTestSlot
	casApplied int
}

func (p *orderCleanerTestPM) IterateSlots(fn func(price float64, slot interface{}) bool) {
	p.mu.Lock()
	snapshots := make(map[float64]orderCleanerTestSlot, len(p.slots))
	for price, slot := range p.slots {
		snapshots[price] = slot
	}
	p.mu.Unlock()

	for price, slot := range snapshots {
		if !fn(price, slot) {
			return
		}
	}
}

func (p *orderCleanerTestPM) CompareAndSwapSlotOrderStatus(
	price float64,
	orderID int64,
	clientOID, expectedStatus, newStatus string,
) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	slot, ok := p.slots[price]
	if !ok || slot.OrderID != orderID || slot.OrderStatus != expectedStatus {
		return false
	}
	if clientOID != "" && slot.ClientOID != clientOID {
		return false
	}
	slot.OrderStatus = newStatus
	p.slots[price] = slot
	p.casApplied++
	return true
}

func (p *orderCleanerTestPM) mutate(price float64, fn func(*orderCleanerTestSlot)) {
	p.mu.Lock()
	slot := p.slots[price]
	fn(&slot)
	p.slots[price] = slot
	p.mu.Unlock()
}

func (p *orderCleanerTestPM) read(price float64) orderCleanerTestSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slots[price]
}

type interleavingCancelExecutor struct {
	orderIDs []int64
	after    func()
}

func (e *interleavingCancelExecutor) BatchCancelOrders(orderIDs []int64) error {
	e.orderIDs = append([]int64(nil), orderIDs...)
	if e.after != nil {
		e.after()
	}
	return nil
}

func TestOrderCleanerCancelResultUsesOrderIdentityCAS(t *testing.T) {
	const (
		price     = 100.0
		oldID     = int64(41)
		oldClient = "10000_B_1700000000001"
	)

	tests := []struct {
		name        string
		interleave  func(*orderCleanerTestPM)
		want        orderCleanerTestSlot
		wantApplied int
	}{
		{
			name: "unchanged order is marked cancel requested",
			want: orderCleanerTestSlot{
				OrderID: oldID, ClientOID: oldClient, OrderSide: "BUY",
				OrderStatus: "CANCEL_REQUESTED",
			},
			wantApplied: 1,
		},
		{
			name: "websocket terminal clear wins",
			interleave: func(pm *orderCleanerTestPM) {
				pm.mutate(price, func(slot *orderCleanerTestSlot) {
					slot.OrderID = 0
					slot.ClientOID = ""
					slot.OrderSide = ""
					slot.OrderStatus = "CANCELED"
				})
			},
			want:        orderCleanerTestSlot{OrderStatus: "CANCELED"},
			wantApplied: 0,
		},
		{
			name: "same price replacement order wins",
			interleave: func(pm *orderCleanerTestPM) {
				pm.mutate(price, func(slot *orderCleanerTestSlot) {
					slot.OrderID = 99
					slot.ClientOID = "10000_B_1700000000099"
					slot.OrderSide = "BUY"
					slot.OrderStatus = "CONFIRMED"
				})
			},
			want: orderCleanerTestSlot{
				OrderID: 99, ClientOID: "10000_B_1700000000099", OrderSide: "BUY",
				OrderStatus: "CONFIRMED",
			},
			wantApplied: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := &orderCleanerTestPM{slots: map[float64]orderCleanerTestSlot{
				price: {
					OrderID: oldID, ClientOID: oldClient, OrderSide: "BUY",
					OrderStatus: "CONFIRMED",
				},
			}}
			executor := &interleavingCancelExecutor{}
			if tt.interleave != nil {
				executor.after = func() { tt.interleave(pm) }
			}
			cfg := &config.Config{}
			cfg.Trading.OrderCleanupThreshold = 1
			cfg.Trading.CleanupBatchSize = 1

			NewOrderCleaner(cfg, executor, pm).CleanupOrders()

			if !reflect.DeepEqual(executor.orderIDs, []int64{oldID}) {
				t.Fatalf("canceled order IDs = %v, want [%d]", executor.orderIDs, oldID)
			}
			if got := pm.read(price); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("slot after interleaving = %+v, want %+v", got, tt.want)
			}
			if pm.casApplied != tt.wantApplied {
				t.Fatalf("applied CAS count = %d, want %d", pm.casApplied, tt.wantApplied)
			}
		})
	}
}
