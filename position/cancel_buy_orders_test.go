package position

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/utils"
)

type cancelBuyTestExecutor struct {
	stubExecutor
	err   error
	calls [][]int64
}

func (e *cancelBuyTestExecutor) BatchCancelOrders(orderIDs []int64) error {
	e.calls = append(e.calls, append([]int64(nil), orderIDs...))
	return e.err
}

type cancelBuyTestOrder struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Type          string
	Status        string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	AvgPrice      float64
	UpdateTime    int64
}

type cancelBuyTestExchange struct {
	stubEx
	openOrders   interface{}
	openSequence []interface{}
	order        interface{}
	openErr      error
	orderErr     error
	openCalls    int
	orderCalls   int
}

type binanceCancelBuyTestExchange struct {
	*cancelBuyTestExchange
}

type lookupCancelBuyTestExchange struct {
	*cancelBuyTestExchange
	lookupErr   error
	lookupCalls int
}

func (e *lookupCancelBuyTestExchange) GetOrderByClientID(context.Context, string, string) (interface{}, error) {
	e.lookupCalls++
	return nil, e.lookupErr
}

func (e *binanceCancelBuyTestExchange) GetName() string { return "Binance" }

func (e *cancelBuyTestExchange) GetOpenOrders(context.Context, string) (interface{}, error) {
	index := e.openCalls
	e.openCalls++
	if len(e.openSequence) > 0 {
		if index >= len(e.openSequence) {
			index = len(e.openSequence) - 1
		}
		return e.openSequence[index], e.openErr
	}
	return e.openOrders, e.openErr
}

func (e *cancelBuyTestExchange) GetOrder(context.Context, string, int64) (interface{}, error) {
	e.orderCalls++
	return e.order, e.orderErr
}

func newCancelBuyTestManager(executor OrderExecutorInterface, ex IExchange) (*SuperPositionManager, *InventorySlot, string) {
	spm := NewSuperPositionManager(testConfig(), executor, ex, 2, 3)
	clientOrderID := spm.generateClientOrderID(100, "BUY")
	slot := spm.getOrCreateSlot(100)
	slot.OrderID = 77
	slot.ClientOID = clientOrderID
	slot.OrderSide = "BUY"
	slot.OrderStatus = OrderStatusConfirmed
	slot.OrderPrice = 100
	slot.OrderQuantity = 0.01
	slot.SlotStatus = SlotStatusLocked
	return spm, slot, clientOrderID
}

func TestCancelAllBuyOrdersConvergesLostTerminalCallback(t *testing.T) {
	executor := &cancelBuyTestExecutor{}
	exchange := &cancelBuyTestExchange{}
	spm, slot, clientOrderID := newCancelBuyTestManager(executor, exchange)
	exchange.openOrders = []*cancelBuyTestOrder{}
	exchange.order = &cancelBuyTestOrder{
		OrderID:       77,
		ClientOrderID: clientOrderID,
		Symbol:        "ETHUSDT",
		Side:          "BUY",
		Type:          "LIMIT",
		Status:        "CANCELED",
		Price:         100,
		Quantity:      0.01,
		ExecutedQty:   0.003,
		AvgPrice:      100,
		UpdateTime:    1_700_000_000_000,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := spm.cancelAllBuyOrders(ctx); err != nil {
		t.Fatalf("cancelAllBuyOrders() error = %v", err)
	}
	if len(executor.calls) != 1 || !slices.Equal(executor.calls[0], []int64{77}) {
		t.Fatalf("cancel calls = %v, want [[77]]", executor.calls)
	}
	if exchange.openCalls != 1 || exchange.orderCalls != 1 {
		t.Fatalf("remote calls = open:%d order:%d, want 1/1", exchange.openCalls, exchange.orderCalls)
	}

	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 0 || slot.OrderStatus != OrderStatusCanceled || slot.SlotStatus != SlotStatusFree {
		t.Fatalf("slot order state = id:%d status:%s slot:%s, want converged canceled/free",
			slot.OrderID, slot.OrderStatus, slot.SlotStatus)
	}
	if slot.PositionStatus != PositionStatusFilled || mathAbs(slot.PositionQty-0.003) > fillQtyTolerance {
		t.Fatalf("slot position = %s/%.12f, want FILLED/0.003", slot.PositionStatus, slot.PositionQty)
	}
	if got := spm.GetTotalBuyQty(); mathAbs(got-0.003) > fillQtyTolerance {
		t.Fatalf("total buy qty = %.12f, want 0.003", got)
	}
}

func TestCancelAllBuyOrdersPropagatesUnconfirmedRemoteState(t *testing.T) {
	cancelErr := errors.New("cancel transport failed")
	executor := &cancelBuyTestExecutor{err: cancelErr}
	exchange := &cancelBuyTestExchange{
		openOrders: []*cancelBuyTestOrder{{
			OrderID: 77, ClientOrderID: "10000_B_1700000000001", Symbol: "ETHUSDT",
			Side: "BUY", Type: "LIMIT", Status: "NEW", Price: 100, Quantity: 0.01,
		}},
	}
	spm, slot, _ := newCancelBuyTestManager(executor, exchange)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := spm.cancelAllBuyOrders(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cancelErr) {
		t.Fatalf("cancelAllBuyOrders() error = %v, want deadline and cancel error", err)
	}
	if !strings.Contains(err.Error(), "77") {
		t.Fatalf("error = %v, want pending order ID", err)
	}

	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 77 || slot.OrderStatus != OrderStatusCancelRequested || slot.SlotStatus != SlotStatusLocked {
		t.Fatalf("uncertain slot was released: id=%d status=%s slot=%s",
			slot.OrderID, slot.OrderStatus, slot.SlotStatus)
	}
}

func TestCancelAllBuyOrdersResolvesUnknownReservationByClientOID(t *testing.T) {
	executor := &cancelBuyTestExecutor{}
	exchange := &cancelBuyTestExchange{}
	spm, slot, clientOrderID := newCancelBuyTestManager(executor, exchange)
	slot.OrderID = 0
	slot.OrderStatus = OrderStatusNotPlaced
	slot.SlotStatus = SlotStatusPending
	exchange.openSequence = []interface{}{
		[]*cancelBuyTestOrder{{
			OrderID: 88, ClientOrderID: clientOrderID, Symbol: "ETHUSDT",
			Side: "BUY", Type: "LIMIT", Status: "NEW", Price: 100, Quantity: 0.01,
		}},
		[]*cancelBuyTestOrder{},
	}
	exchange.order = &cancelBuyTestOrder{
		OrderID: 88, ClientOrderID: clientOrderID, Symbol: "ETHUSDT",
		Side: "BUY", Type: "LIMIT", Status: "CANCELED", Price: 100, Quantity: 0.01,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := spm.cancelAllBuyOrders(ctx); err != nil {
		t.Fatalf("cancelAllBuyOrders() error = %v", err)
	}
	if len(executor.calls) != 1 || !slices.Equal(executor.calls[0], []int64{88}) {
		t.Fatalf("cancel calls = %v, want [[88]]", executor.calls)
	}
	if exchange.openCalls != 2 || exchange.orderCalls != 1 {
		t.Fatalf("remote calls = open:%d order:%d, want 2/1", exchange.openCalls, exchange.orderCalls)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 0 || slot.ClientOID != "" || slot.SlotStatus != SlotStatusFree {
		t.Fatalf("resolved UNKNOWN did not converge: id=%d client=%q slot=%s",
			slot.OrderID, slot.ClientOID, slot.SlotStatus)
	}
}

func TestCancelAllBuyOrdersKeepsUnknownReservationWithoutTerminalEvidence(t *testing.T) {
	executor := &cancelBuyTestExecutor{}
	exchange := &cancelBuyTestExchange{openOrders: []*cancelBuyTestOrder{}}
	spm, slot, clientOrderID := newCancelBuyTestManager(executor, exchange)
	slot.OrderID = 0
	slot.OrderStatus = OrderStatusNotPlaced
	slot.SlotStatus = SlotStatusPending

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := spm.cancelAllBuyOrders(ctx)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN") || !strings.Contains(err.Error(), clientOrderID) {
		t.Fatalf("cancelAllBuyOrders() error = %v, want unresolved UNKNOWN identity", err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("unknown order without OrderID was sent to cancel: %v", executor.calls)
	}
	if exchange.orderCalls != 0 {
		t.Fatalf("GetOrder called without an authoritative OrderID: %d", exchange.orderCalls)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 0 || slot.ClientOID != clientOrderID || slot.SlotStatus != SlotStatusPending ||
		slot.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("UNKNOWN reservation was unsafely released: id=%d client=%q slot=%s status=%s",
			slot.OrderID, slot.ClientOID, slot.SlotStatus, slot.OrderStatus)
	}
}

func TestCancelAllBuyOrdersReleasesUnknownAfterRepeatedExplicitAbsence(t *testing.T) {
	executor := &cancelBuyTestExecutor{}
	baseExchange := &cancelBuyTestExchange{openOrders: []*cancelBuyTestOrder{}}
	exchange := &lookupCancelBuyTestExchange{
		cancelBuyTestExchange: baseExchange,
		lookupErr:             exchangeerr.ErrOrderNotFound,
	}
	spm, slot, _ := newCancelBuyTestManager(executor, exchange)
	slot.OrderID = 0
	slot.OrderStatus = OrderStatusNotPlaced
	slot.OrderCreatedAt = time.Now().Add(-pendingLookupMinAge - time.Second)
	slot.SlotStatus = SlotStatusPending

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := spm.cancelAllBuyOrders(ctx); err != nil {
		t.Fatalf("cancelAllBuyOrders() error = %v", err)
	}
	if exchange.lookupCalls != pendingLookupMissLimit {
		t.Fatalf("ClientOID lookup calls = %d, want %d", exchange.lookupCalls, pendingLookupMissLimit)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("unknown absent order was sent to cancel: %v", executor.calls)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.SlotStatus != SlotStatusFree || slot.ClientOID != "" || slot.OrderID != 0 {
		t.Fatalf("confirmed absent reservation did not converge: %+v", slot)
	}
}

func TestUnknownReservationAcceptsPrefixedTerminalReplay(t *testing.T) {
	executor := &cancelBuyTestExecutor{}
	baseExchange := &cancelBuyTestExchange{}
	exchange := &binanceCancelBuyTestExchange{cancelBuyTestExchange: baseExchange}
	spm, slot, clientOrderID := newCancelBuyTestManager(executor, exchange)
	slot.OrderID = 0
	slot.OrderStatus = OrderStatusNotPlaced
	slot.SlotStatus = SlotStatusPending

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       88,
		ClientOrderID: utils.AddBinanceBrokerPrefix(clientOrderID),
		Symbol:        "ETHUSDT",
		Side:          "BUY",
		Status:        "CANCELED",
		Price:         100,
	})

	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 0 || slot.ClientOID != "" || slot.SlotStatus != SlotStatusFree ||
		slot.OrderStatus != OrderStatusCanceled {
		t.Fatalf("prefixed terminal replay did not converge UNKNOWN: id=%d client=%q slot=%s status=%s",
			slot.OrderID, slot.ClientOID, slot.SlotStatus, slot.OrderStatus)
	}
}

func TestCancelAllBuyOrdersRequiresTerminalOrderDetails(t *testing.T) {
	detailErr := errors.New("order history unavailable")
	executor := &cancelBuyTestExecutor{}
	exchange := &cancelBuyTestExchange{
		openOrders: []*cancelBuyTestOrder{},
		orderErr:   detailErr,
	}
	spm, slot, _ := newCancelBuyTestManager(executor, exchange)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := spm.cancelAllBuyOrders(ctx)
	if !errors.Is(err, detailErr) {
		t.Fatalf("cancelAllBuyOrders() error = %v, want detail error", err)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 77 || slot.SlotStatus != SlotStatusLocked {
		t.Fatalf("slot converged without terminal details: id=%d slot=%s", slot.OrderID, slot.SlotStatus)
	}
}

func mathAbs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
