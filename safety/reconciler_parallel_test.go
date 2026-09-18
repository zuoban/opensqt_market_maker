package safety

import (
	"context"
	"errors"
	"testing"
	"time"
)

type parallelReadExchange struct {
	positions func(context.Context) (interface{}, error)
	orders    func(context.Context) (interface{}, error)
}

func (e parallelReadExchange) GetPositions(ctx context.Context, _ string) (interface{}, error) {
	return e.positions(ctx)
}
func (e parallelReadExchange) GetOpenOrders(ctx context.Context, _ string) (interface{}, error) {
	return e.orders(ctx)
}
func (e parallelReadExchange) GetBaseAsset() string { return "ETH" }

func TestReconcileRemoteReadsOverlap(t *testing.T) {
	positionsStarted := make(chan struct{})
	ordersStarted := make(chan struct{})
	ex := parallelReadExchange{
		positions: func(ctx context.Context) (interface{}, error) {
			close(positionsStarted)
			select {
			case <-ordersStarted:
				return []*reconcileTestPosition{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		orders: func(ctx context.Context) (interface{}, error) {
			close(ordersStarted)
			select {
			case <-positionsStarted:
				return []*reconcileTestOrder{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := NewReconciler(nil, ex, nil)
	positions, orders, err := r.readRemoteState(ctx, "ETHUSDT")
	if err != nil || positions == nil || orders == nil {
		t.Fatalf("parallel reads failed: positions=%v orders=%v err=%v", positions, orders, err)
	}
}

func TestReconcileReadFailureCancelsOtherRequestAndStaysUnhealthy(t *testing.T) {
	for _, failPositions := range []bool{false, true} {
		t.Run(map[bool]string{true: "positions", false: "orders"}[failPositions], func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			want := errors.New("read failed")
			blocked := func(ctx context.Context) (interface{}, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				return nil, ctx.Err()
			}
			failed := func(context.Context) (interface{}, error) { <-started; return nil, want }
			ex := parallelReadExchange{positions: blocked, orders: failed}
			if failPositions {
				ex.positions, ex.orders = failed, blocked
			}
			pm := &reconcileTestPM{}
			r := NewReconciler(nil, ex, pm)
			r.healthy.Store(true)
			if err := r.Reconcile(); !errors.Is(err, want) || r.IsHealthy() || pm.reconcileCount != 0 {
				t.Fatalf("partial read was accepted: err=%v healthy=%v count=%d", err, r.IsHealthy(), pm.reconcileCount)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("remaining query was not canceled")
			}
		})
	}
}
