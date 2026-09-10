package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opensqt/telemetry"
)

func TestPerformanceRouteRequiresDashboardToken(t *testing.T) {
	s := startServer(t, "performance-test-token")
	defer s.Shutdown(time.Second)
	for _, authorized := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/performance", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusUnauthorized
		if authorized {
			req.Header.Set("X-Dashboard-Token", "performance-test-token")
			want = http.StatusServiceUnavailable
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("authorized=%v: status %d want %d", authorized, response.StatusCode, want)
		}
	}
}

func TestPerformanceEndpointOnlyReadsCachedSnapshot(t *testing.T) {
	r := telemetry.New(time.Now())
	collections := 0
	r.SetStateProvider(func() telemetry.StateCounts { collections++; return telemetry.StateCounts{Slots: 42} })
	s := New(Options{Performance: r})
	if s.assembler.performance != r {
		t.Fatal("recorder not wired")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/performance", nil)
	unready := httptest.NewRecorder()
	s.handlePerformance(unready, request)
	if unready.Code != http.StatusServiceUnavailable {
		t.Fatal("uncollected endpoint returned ready")
	}
	r.Observe(telemetry.Planning, 2*time.Millisecond, false)
	r.Refresh()
	r.Observe(telemetry.Planning, time.Second, false)
	for i := 0; i < 3; i++ {
		response := httptest.NewRecorder()
		s.handlePerformance(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status/headers: %+v", response.Result())
		}
		var got telemetry.Snapshot
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Ready || got.State.Slots != 42 || got.Latencies[telemetry.Planning].Count != 1 || got.Latencies[telemetry.Planning].P95MS != 2 {
			t.Fatalf("endpoint didn't use cached snapshot: %+v", got)
		}
		full := s.assembler.Build()
		if full.Performance == nil || full.Performance.SampledAt != r.Snapshot().SampledAt {
			t.Fatal("full snapshot missing same performance view")
		}
		full.Performance.State.Slots = 0
	}
	if collections != 1 || r.Snapshot().State.Slots != 42 {
		t.Fatal("dashboard collected state or mutated cache")
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response := httptest.NewRecorder()
		s.handlePerformance(response, httptest.NewRequest(method, "/api/performance", nil))
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET" {
			t.Fatalf("%s was accepted", method)
		}
	}
}
