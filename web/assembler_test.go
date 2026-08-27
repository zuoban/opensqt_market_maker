package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/safety"
)

func TestSafeAppViewOmitsSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.CurrentExchange = "bitget"
	cfg.Trading.Symbol = "ETHUSDT"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.MaxMarginUsagePercent = 62.5
	cfg.Exchanges = map[string]config.ExchangeConfig{
		"bitget": {
			APIKey:     "SECRETKEY_ABC",
			SecretKey:  "SUPERSECRET_XYZ",
			Passphrase: "PASSPHRASE_123",
			FeeRate:    0.0002,
		},
	}
	cfg.RiskControl.Enabled = true
	cfg.RiskControl.MonitorSymbols = []string{"BTCUSDT"}

	view := safeAppView(cfg)
	if view.FeeRate != 0.0002 || view.Exchange != "bitget" || view.MaxMarginUsage != 62.5 {
		t.Fatalf("view = %+v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"SECRETKEY_ABC", "SUPERSECRET_XYZ", "PASSPHRASE_123", "api_key", "secret_key", "passphrase"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(secret)) && (secret == "SECRETKEY_ABC" || secret == "SUPERSECRET_XYZ" || secret == "PASSPHRASE_123") {
			t.Fatalf("secret leaked in %s", text)
		}
	}
	if strings.Contains(text, "SECRETKEY_ABC") || strings.Contains(text, "SUPERSECRET_XYZ") || strings.Contains(text, "PASSPHRASE_123") {
		t.Fatalf("secret leaked: %s", text)
	}

	a := &assembler{cfg: cfg, version: "test"}
	full, err := json.Marshal(a.Build())
	if err != nil {
		t.Fatal(err)
	}
	body := string(full)
	if strings.Contains(body, "SECRETKEY_ABC") || strings.Contains(body, "SUPERSECRET_XYZ") || strings.Contains(body, "PASSPHRASE_123") {
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
	snapshot := a.Build()

	if !snapshot.StartedAt.Equal(started) {
		t.Fatalf("startedAt = %v, want %v", snapshot.StartedAt, started)
	}
	if snapshot.UptimeSec < 94 || snapshot.UptimeSec > 96 {
		t.Fatalf("uptimeSec = %f", snapshot.UptimeSec)
	}
}
