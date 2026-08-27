package exchange

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"opensqt/exchange/bitget"
)

type bitgetCancellationBackendStub struct {
	openOrders         []*bitget.Order
	getOpenOrdersErr   error
	batchCancelErr     error
	queriedSymbols     []string
	batchCancelSymbols []string
	batchCancelIDs     [][]int64
	productCancelCalls int
}

func (s *bitgetCancellationBackendStub) GetOpenOrders(_ context.Context, symbol string) ([]*bitget.Order, error) {
	s.queriedSymbols = append(s.queriedSymbols, symbol)
	return s.openOrders, s.getOpenOrdersErr
}

func (s *bitgetCancellationBackendStub) BatchCancelOrders(_ context.Context, symbol string, orderIDs []int64) error {
	s.batchCancelSymbols = append(s.batchCancelSymbols, symbol)
	s.batchCancelIDs = append(s.batchCancelIDs, append([]int64(nil), orderIDs...))
	return s.batchCancelErr
}

func (s *bitgetCancellationBackendStub) CancelAllOrders(context.Context) error {
	s.productCancelCalls++
	return nil
}

func TestBitgetWrapperCancelAllOrdersIsScopedToSymbol(t *testing.T) {
	backend := &bitgetCancellationBackendStub{
		openOrders: []*bitget.Order{
			{OrderID: 101, Symbol: "BTCUSDT"},
			{OrderID: 202, Symbol: "ETHUSDT"},
			nil,
			{OrderID: 0, Symbol: "BTCUSDT"},
			{OrderID: 103, Symbol: "btcusdt_umcbl"},
		},
	}
	wrapper := &bitgetWrapper{cancellationBackend: backend}

	if err := wrapper.CancelAllOrders(context.Background(), "BTCUSDT"); err != nil {
		t.Fatalf("CancelAllOrders() error = %v", err)
	}
	if !reflect.DeepEqual(backend.queriedSymbols, []string{"BTCUSDT"}) {
		t.Fatalf("queried symbols = %#v", backend.queriedSymbols)
	}
	if !reflect.DeepEqual(backend.batchCancelSymbols, []string{"BTCUSDT"}) {
		t.Fatalf("batch cancel symbols = %#v", backend.batchCancelSymbols)
	}
	if !reflect.DeepEqual(backend.batchCancelIDs, [][]int64{{101, 103}}) {
		t.Fatalf("batch cancel IDs = %#v", backend.batchCancelIDs)
	}
	if backend.productCancelCalls != 0 {
		t.Fatalf("product-level CancelAllOrders called %d times", backend.productCancelCalls)
	}
}

func TestBitgetWrapperCancelAllOrdersSkipsOtherSymbols(t *testing.T) {
	backend := &bitgetCancellationBackendStub{
		openOrders: []*bitget.Order{{OrderID: 202, Symbol: "ETHUSDT"}},
	}
	wrapper := &bitgetWrapper{cancellationBackend: backend}

	if err := wrapper.CancelAllOrders(context.Background(), "BTCUSDT"); err != nil {
		t.Fatalf("CancelAllOrders() error = %v", err)
	}
	if len(backend.batchCancelIDs) != 0 {
		t.Fatalf("unexpected batch cancel IDs = %#v", backend.batchCancelIDs)
	}
	if backend.productCancelCalls != 0 {
		t.Fatalf("product-level CancelAllOrders called %d times", backend.productCancelCalls)
	}
}

func TestBitgetWrapperCancelAllOrdersPropagatesScopedOperationErrors(t *testing.T) {
	queryErr := errors.New("query failed")
	backend := &bitgetCancellationBackendStub{getOpenOrdersErr: queryErr}
	wrapper := &bitgetWrapper{cancellationBackend: backend}
	if err := wrapper.CancelAllOrders(context.Background(), "BTCUSDT"); !errors.Is(err, queryErr) {
		t.Fatalf("query error = %v, want %v", err, queryErr)
	}
	if len(backend.batchCancelIDs) != 0 || backend.productCancelCalls != 0 {
		t.Fatalf("calls after query error: batch=%#v product=%d", backend.batchCancelIDs, backend.productCancelCalls)
	}

	batchErr := errors.New("batch cancel failed")
	backend = &bitgetCancellationBackendStub{
		openOrders:     []*bitget.Order{{OrderID: 101, Symbol: "BTCUSDT"}},
		batchCancelErr: batchErr,
	}
	wrapper = &bitgetWrapper{cancellationBackend: backend}
	if err := wrapper.CancelAllOrders(context.Background(), "BTCUSDT"); !errors.Is(err, batchErr) {
		t.Fatalf("batch error = %v, want %v", err, batchErr)
	}
	if backend.productCancelCalls != 0 {
		t.Fatalf("product-level CancelAllOrders called %d times", backend.productCancelCalls)
	}
}
