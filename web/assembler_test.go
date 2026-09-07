package web

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/position"
	"opensqt/safety"
)

type blockingPositionSnapshotSource struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingPositionSnapshotSource) Snapshot() position.PositionSnapshot {
	close(s.started)
	<-s.release
	return position.PositionSnapshot{Initialized: true, Symbol: "SOLUSDC"}
}

func TestSafeAppViewOmitsSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trading.Symbol = "ETHUSDT"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.MaxMarginUsagePercent = 62.5
	cfg.Exchanges.Binance = config.BinanceConfig{
		APIKey:    "SECRETKEY_ABC",
		SecretKey: "SUPERSECRET_XYZ",
		FeeRate:   0.0002,
	}
	cfg.RiskControl.Enabled = true
	cfg.RiskControl.MonitorSymbols = []string{"BTCUSDT"}
	cfg.Dashboard.PushIntervalMS = 400

	view := safeAppView(cfg)
	if view.FeeRate != 0.0002 || view.Exchange != "binance" || view.MaxMarginUsage != 62.5 ||
		view.DashboardPushIntervalMS != 400 {
		t.Fatalf("view = %+v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"SECRETKEY_ABC", "SUPERSECRET_XYZ", "api_key", "secret_key"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(secret)) && (secret == "SECRETKEY_ABC" || secret == "SUPERSECRET_XYZ") {
			t.Fatalf("secret leaked in %s", text)
		}
	}
	if strings.Contains(text, "SECRETKEY_ABC") || strings.Contains(text, "SUPERSECRET_XYZ") {
		t.Fatalf("secret leaked: %s", text)
	}

	a := &assembler{cfg: cfg, version: "test"}
	full, err := json.Marshal(a.Build())
	if err != nil {
		t.Fatal(err)
	}
	body := string(full)
	if strings.Contains(body, "SECRETKEY_ABC") || strings.Contains(body, "SUPERSECRET_XYZ") {
		t.Fatalf("full snapshot leaked secrets: %s", body)
	}
}

func TestSnapshotIncludesMarginGuard(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trading.MaxMarginUsagePercent = 40
	source := &mockAccount{acc: &exchange.Account{
		TotalWalletBalance: 100,
		TotalMarginBalance: 100,
		AvailableBalance:   55,
	}}
	margin := safety.NewMarginMonitor(cfg, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := margin.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		margin.Stop()
	})

	snapshot := (&assembler{margin: margin}).Build()
	if !snapshot.Margin.Ready || !snapshot.Margin.Triggered {
		t.Fatalf("margin snapshot = %+v", snapshot.Margin)
	}
	if snapshot.Margin.UsagePercent != 45 || snapshot.Margin.LimitPercent != 40 ||
		snapshot.Margin.UsedMargin != 45 || snapshot.Margin.QuoteAsset != "USDT" {
		t.Fatalf("margin snapshot = %+v", snapshot.Margin)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"margin\"", "\"usagePercent\":45", "\"limitPercent\":40", "\"triggered\":true"} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("snapshot JSON missing %s: %s", field, raw)
		}
	}
}

func TestSnapshotIncludesProgramStartAndUptime(t *testing.T) {
	started := time.Now().Add(-95 * time.Second)
	a := &assembler{version: "test", started: started}
	first := a.Build()
	second := a.Build()

	if !first.StartedAt.Equal(started) {
		t.Fatalf("startedAt = %v, want %v", first.StartedAt, started)
	}
	if first.UptimeSec < 94 || first.UptimeSec > 96 {
		t.Fatalf("uptimeSec = %f", first.UptimeSec)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("snapshot sequences = %d, %d; want 1, 2", first.Sequence, second.Sequence)
	}
}

func TestSnapshotSequenceIsUniqueAcrossConcurrentBuilds(t *testing.T) {
	const count = 64
	a := &assembler{started: time.Now()}
	sequences := make(chan uint64, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sequences <- a.Build().Sequence
		}()
	}
	wg.Wait()
	close(sequences)

	seen := make(map[uint64]struct{}, count)
	for sequence := range sequences {
		if sequence == 0 {
			t.Fatal("snapshot sequence must be positive")
		}
		if _, exists := seen[sequence]; exists {
			t.Fatalf("duplicate snapshot sequence %d", sequence)
		}
		seen[sequence] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("snapshot sequence count = %d, want %d", len(seen), count)
	}
}

func TestAssemblerBuildDoesNotWaitForBlockedPositionRefresh(t *testing.T) {
	source := &blockingPositionSnapshotSource{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	cache := newPositionCache(source, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		cache.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		close(source.release)
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("position cache did not stop")
		}
	})

	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("position refresh did not start")
	}

	result := make(chan *Snapshot, 1)
	go func() {
		result <- (&assembler{position: cache}).Build()
	}()

	select {
	case snapshot := <-result:
		if snapshot.PositionReady || !snapshot.PositionUpdatedAt.IsZero() || snapshot.PositionAgeMs != 0 {
			t.Fatalf("position metadata before first refresh = ready:%v updated:%v age:%d",
				snapshot.PositionReady, snapshot.PositionUpdatedAt, snapshot.PositionAgeMs)
		}
		if !reflect.DeepEqual(snapshot.Position, position.PositionSnapshot{}) {
			t.Fatalf("position before first refresh = %+v", snapshot.Position)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("assembler Build waited for blocked position refresh")
	}
}
