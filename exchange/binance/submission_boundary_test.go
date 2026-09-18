package binance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/utils"
)

func TestCreateBoundaryUnlocksBeforeSlowConfirmation(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, outcome := range []string{"accepted", "retry", "stale"} {
			t.Run(fmt.Sprintf("batch=%v/%s", native, outcome), func(t *testing.T) {
				n := 1
				if native {
					n = 2
				}
				var held, posts, begins atomic.Int32
				var invalid atomic.Bool
				queryStarted := make(chan struct{}, n)
				finishQuery := make(chan struct{})
				var unblock sync.Once
				defer unblock.Do(func() { close(finishQuery) })
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						first := posts.Add(1) == 1
						wantHeld := int32(1)
						if first {
							wantHeld = int32(n)
						}
						if held.Load() != wantHeld {
							t.Errorf("POST lease count=%d want=%d", held.Load(), wantHeld)
						}
						if first {
							writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "unknown"})
							return
						}
						_ = r.ParseForm()
						writeJSON(t, w, http.StatusOK, orderFixture(42, r.Form.Get("newClientOrderId")))
						return
					}
					if held.Load() != 0 {
						t.Errorf("read-only confirmation holds %d leases", held.Load())
					}
					queryStarted <- struct{}{}
					select {
					case <-finishQuery:
					case <-r.Context().Done():
						return
					}
					if outcome == "accepted" {
						writeJSON(t, w, http.StatusOK, orderFixture(42, r.URL.Query().Get("origClientOrderId")))
					} else {
						writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
					}
				}))
				defer server.Close()
				adapter := testAdapter(t, server.URL, "USDT")
				adapter.orderConfirmationTimeout = 2 * time.Second
				requests := make([]*OrderRequest, n)
				locks := make([]sync.Mutex, n)
				for i := range requests {
					req := placementRequest()
					req.ClientOrderID = fmt.Sprintf("boundary-%d", i)
					req.BeginSubmission = func(context.Context) (func(), error) {
						if invalid.Load() {
							return nil, exchangeerr.WrapOrderPlacementUnknown(errors.New("reservation changed"))
						}
						locks[i].Lock()
						begins.Add(1)
						held.Add(1)
						return func() { held.Add(-1); locks[i].Unlock() }, nil
					}
					requests[i] = req
				}
				finished := make(chan error, 1)
				go func() {
					if native {
						items, err := adapter.PlaceOrderBatch(context.Background(), requests)
						for _, item := range items {
							err = errors.Join(err, item.Err)
						}
						finished <- err
					} else {
						_, err := adapter.PlaceOrder(context.Background(), requests[0])
						finished <- err
					}
				}()
				for range n {
					select {
					case <-queryStarted:
					case <-time.After(time.Second):
						unblock.Do(func() { close(finishQuery) })
						t.Fatal("confirmation did not start")
					}
				}
				// 确认请求仍挂起时，各槽位回调已能拿锁，且本批全部 lease 都已释放。
				for i := range locks {
					if !locks[i].TryLock() {
						t.Errorf("slot %d blocked behind confirmation", i)
					} else {
						locks[i].Unlock()
					}
				}
				invalid.Store(outcome == "stale")
				unblock.Do(func() { close(finishQuery) })
				select {
				case err := <-finished:
					if (outcome == "stale") != errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || outcome != "stale" && err != nil {
						t.Fatalf("outcome=%s err=%v", outcome, err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("placement did not finish")
				}
				wantPosts, wantBegins := int32(1), int32(n)
				if outcome == "retry" {
					wantPosts += int32(n)
					wantBegins *= 2
				}
				if posts.Load() != wantPosts || begins.Load() != wantBegins || held.Load() != 0 {
					t.Fatalf("posts=%d begins=%d held=%d", posts.Load(), begins.Load(), held.Load())
				}
			})
		}
	}
}

func TestBatchLocalValidationReleasesLeaseBeforeOtherConfirmations(t *testing.T) {
	var held atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if held.Load() != 1 {
				t.Errorf("invalid member retained lease: %d", held.Load())
			}
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "unknown"})
			return
		}
		if held.Load() != 0 {
			t.Errorf("confirmation retained lease: %d", held.Load())
		}
		writeJSON(t, w, http.StatusOK, orderFixture(42, utils.AddBinanceBrokerPrefix("grid-order-1")))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL, "USDT")
	valid, invalid := placementRequest(), placementRequest()
	invalid.Quantity = -1
	for _, req := range []*OrderRequest{valid, invalid} {
		req.BeginSubmission = func(context.Context) (func(), error) { held.Add(1); return func() { held.Add(-1) }, nil }
	}
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{valid, invalid})
	if err != nil || len(items) != 2 || items[0].Err != nil || !exchangeerr.IsOrderPlacementRejected(items[1].Err) || held.Load() != 0 {
		t.Fatalf("mixed validation: items=%+v err=%v held=%d", items, err, held.Load())
	}
}
