package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	f, err := os.CreateTemp("", "opensqt-empty-env-*")
	if err != nil {
		panic(err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Setenv("OPENSQT_ENV_FILE", name)
	code := m.Run()
	_ = os.Remove(name)
	os.Exit(code)
}

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const envTestYAML = `
app:
  current_exchange: "bitget"
exchanges:
  bitget:
    api_key: "yaml-key"
    secret_key: "yaml-secret"
    passphrase: "yaml-pass"
    fee_rate: 0.0002
trading:
  symbol: "ETHUSDT"
  order_quantity: 30
  buy_window_size: 10
`

func TestEnvOverridesYAMLSecrets(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BITGET_API_KEY", "env-key")
	t.Setenv("OPENSQT_EXCHANGES_BITGET_SECRET_KEY", "env-secret")
	t.Setenv("OPENSQT_EXCHANGES_BITGET_PASSPHRASE", "env-pass")
	t.Setenv("OPENSQT_TRADING_SYMBOL", "BTCUSDC")
	t.Setenv("OPENSQT_DASHBOARD_LISTEN", "0.0.0.0:8787")
	t.Setenv("OPENSQT_DASHBOARD_TOKEN", "panel-token")

	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	ex := cfg.Exchanges["bitget"]
	if ex.APIKey != "env-key" || ex.SecretKey != "env-secret" || ex.Passphrase != "env-pass" {
		t.Fatalf("exchange = %+v", ex)
	}
	if cfg.Trading.Symbol != "BTCUSDC" {
		t.Fatalf("symbol = %s", cfg.Trading.Symbol)
	}
	if cfg.Dashboard.Listen != "0.0.0.0:8787" || cfg.Dashboard.Token != "panel-token" {
		t.Fatalf("dashboard = %+v", cfg.Dashboard)
	}
}

func TestEmptyEnvDoesNotOverrideYAML(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BITGET_API_KEY", "   ")
	t.Setenv("OPENSQT_TRADING_SYMBOL", "")
	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exchanges["bitget"].APIKey != "yaml-key" {
		t.Fatalf("api key = %s", cfg.Exchanges["bitget"].APIKey)
	}
	if cfg.Trading.Symbol != "ETHUSDT" {
		t.Fatalf("symbol = %s", cfg.Trading.Symbol)
	}
}

func TestEnvCreatesMissingExchange(t *testing.T) {
	t.Setenv("OPENSQT_APP_CURRENT_EXCHANGE", "binance")
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_API_KEY", "bnb-key")
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_SECRET_KEY", "bnb-secret")
	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App.CurrentExchange != "binance" {
		t.Fatalf("exchange = %s", cfg.App.CurrentExchange)
	}
	ex := cfg.Exchanges["binance"]
	if ex.APIKey != "bnb-key" || ex.SecretKey != "bnb-secret" {
		t.Fatalf("binance = %+v", ex)
	}
}

func TestInvalidEnvType(t *testing.T) {
	t.Setenv("OPENSQT_TRADING_ORDER_QUANTITY", "not-a-number")
	_, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err == nil {
		t.Fatal("expected invalid number error")
	}
}

func TestParseDotEnv(t *testing.T) {
	kv, err := parseDotEnv(`
# comment
export OPENSQT_DASHBOARD_TOKEN="tok en"
OPENSQT_TRADING_SYMBOL=ETHUSDC # inline
OPENSQT_SYSTEM_LOG_LEVEL='INFO'
`)
	if err != nil {
		t.Fatal(err)
	}
	if kv["OPENSQT_DASHBOARD_TOKEN"] != "tok en" {
		t.Fatalf("token = %q", kv["OPENSQT_DASHBOARD_TOKEN"])
	}
	if kv["OPENSQT_TRADING_SYMBOL"] != "ETHUSDC" {
		t.Fatalf("symbol = %q", kv["OPENSQT_TRADING_SYMBOL"])
	}
	if kv["OPENSQT_SYSTEM_LOG_LEVEL"] != "INFO" {
		t.Fatalf("level = %q", kv["OPENSQT_SYSTEM_LOG_LEVEL"])
	}
}

func TestLoadDotEnvDoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OPENSQT_TRADING_SYMBOL=FROM_FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSQT_ENV_FILE", path)
	t.Setenv("OPENSQT_TRADING_SYMBOL", "FROM_PROCESS")
	if err := LoadDotEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OPENSQT_TRADING_SYMBOL") != "FROM_PROCESS" {
		t.Fatalf("got %s", os.Getenv("OPENSQT_TRADING_SYMBOL"))
	}
}

func TestRiskSymbolsAndBoolFromEnv(t *testing.T) {
	t.Setenv("OPENSQT_RISK_CONTROL_ENABLED", "false")
	t.Setenv("OPENSQT_RISK_CONTROL_MONITOR_SYMBOLS", "BTCUSDT, ETHUSDT")
	t.Setenv("OPENSQT_DASHBOARD_ENABLED", "0")
	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RiskControl.Enabled {
		t.Fatal("risk should be off")
	}
	if len(cfg.RiskControl.MonitorSymbols) != 2 || cfg.RiskControl.MonitorSymbols[0] != "BTCUSDT" {
		t.Fatalf("symbols = %#v", cfg.RiskControl.MonitorSymbols)
	}
	if cfg.DashboardEnabled() {
		t.Fatal("dashboard should be off")
	}
}
