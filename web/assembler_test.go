package web

import (
	"encoding/json"
	"strings"
	"testing"

	"opensqt/config"
)

func TestSafeAppViewOmitsSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.CurrentExchange = "bitget"
	cfg.Trading.Symbol = "ETHUSDT"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 30
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
	if view.FeeRate != 0.0002 || view.Exchange != "bitget" {
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
