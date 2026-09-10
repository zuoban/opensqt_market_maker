package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestExchangeRateCacheFetchesOnceAndServesMemoryView(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2026-09-09","rates":{"CNY":6.7078}}`))
	}))
	defer server.Close()

	cache := newExchangeRateCache(server.Client(), server.URL, time.Hour, time.Minute)
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		view := cache.View()
		if !view.Ready || view.Stale || view.CNYPerUSD != 6.7078 || view.Source != "Frankfurter" || view.RateDate != "2026-09-09" {
			t.Fatalf("view = %+v", view)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestExchangeRateCacheKeepsLastSuccessWhenRefreshFails(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"base":"USD","date":"2026-09-09","rates":{"CNY":6.7078}}`))
	}))
	defer server.Close()

	cache := newExchangeRateCache(server.Client(), server.URL, time.Hour, time.Minute)
	if err := cache.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := cache.refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}

	view := cache.View()
	if !view.Ready || !view.Stale || view.CNYPerUSD != 6.7078 || view.Error == "" {
		t.Fatalf("stale view = %+v", view)
	}
}

func TestExchangeRateCacheRejectsInvalidRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"base":"USD","date":"2026-09-09","rates":{"CNY":72}}`))
	}))
	defer server.Close()

	cache := newExchangeRateCache(server.Client(), server.URL, time.Hour, time.Minute)
	if err := cache.refresh(context.Background()); err == nil {
		t.Fatal("expected invalid rate error")
	}
	view := cache.View()
	if view.Ready || !view.Stale || view.CNYPerUSD != 0 {
		t.Fatalf("invalid view = %+v", view)
	}
}
