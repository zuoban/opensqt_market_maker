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
exchanges:
  binance:
    api_key: "yaml-key"
    secret_key: "yaml-secret"
    fee_rate: 0.0002
trading:
  symbol: "ETHUSDT"
  order_quantity: 30
  buy_window_size: 10
`

func TestEnvOverridesYAMLSecrets(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_API_KEY", "env-key")
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_SECRET_KEY", "env-secret")
	t.Setenv("OPENSQT_TRADING_SYMBOL", "BTCUSDC")
	t.Setenv("OPENSQT_DASHBOARD_LISTEN", "0.0.0.0:8787")
	t.Setenv("OPENSQT_DASHBOARD_TOKEN", "panel-token")

	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	ex := cfg.Exchanges.Binance
	if ex.APIKey != "env-key" || ex.SecretKey != "env-secret" {
		t.Fatalf("exchange = %+v", ex)
	}
	if cfg.Trading.Symbol != "BTCUSDC" {
		t.Fatalf("symbol = %s", cfg.Trading.Symbol)
	}
	if cfg.Dashboard.Listen != "0.0.0.0:8787" || cfg.Dashboard.Token != "panel-token" {
		t.Fatalf("dashboard = %+v", cfg.Dashboard)
	}
}

func TestExecutionEnvOverrides(t *testing.T) {
	t.Setenv("OPENSQT_EXECUTION_MAKER_GUARD_TICKS", "3")
	t.Setenv("OPENSQT_EXECUTION_QUOTE_STALE_MS", "2500")
	t.Setenv("OPENSQT_EXECUTION_POST_ONLY_RETRY_MIN_MS", "75")
	t.Setenv("OPENSQT_EXECUTION_POST_ONLY_RETRY_MAX_MS", "600")
	t.Setenv("OPENSQT_EXECUTION_POST_ONLY_RETRY_BURST", "7")
	t.Setenv("OPENSQT_EXECUTION_CATCH_UP_MODE", "exact_wait")
	t.Setenv("OPENSQT_EXECUTION_MAX_ACTIVE_CATCH_UP_SLOTS", "2")
	t.Setenv("OPENSQT_EXECUTION_MAX_CATCH_UP_SLOTS_PER_ADJUST", "2")
	t.Setenv("OPENSQT_EXECUTION_MAX_CATCH_UP_DISTANCE_RATIO", "0.4")
	t.Setenv("OPENSQT_EXECUTION_NEAR_TOUCH_SINGLE_ORDER_RATIO", "0.2")

	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Execution
	if got.MakerGuardTicks != 3 || got.QuoteStaleMS != 2500 ||
		got.PostOnlyRetryMinMS != 75 || got.PostOnlyRetryMaxMS != 600 ||
		got.PostOnlyRetryBurst != 7 || got.CatchUpMode != "exact_wait" ||
		got.MaxActiveCatchUpSlots != 2 || got.MaxCatchUpSlotsPerAdjust != 2 ||
		got.MaxCatchUpDistanceRatio != 0.4 || got.NearTouchSingleOrderRatio != 0.2 {
		t.Fatalf("execution overrides = %+v", got)
	}
}

func TestEmptyEnvDoesNotOverrideYAML(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_API_KEY", "   ")
	t.Setenv("OPENSQT_TRADING_SYMBOL", "")
	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exchanges.Binance.APIKey != "yaml-key" {
		t.Fatalf("api key = %s", cfg.Exchanges.Binance.APIKey)
	}
	if cfg.Trading.Symbol != "ETHUSDT" {
		t.Fatalf("symbol = %s", cfg.Trading.Symbol)
	}
}

func TestLegacyExchangeSelectorDoesNotAffectBinance(t *testing.T) {
	cfg, err := LoadConfig(writeTempYAML(t, `
app:
  current_exchange: "legacy"
`+envTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exchanges.Binance.APIKey != "yaml-key" {
		t.Fatalf("Binance config = %+v", cfg.Exchanges.Binance)
	}
}

func TestEnvCreatesMissingBinanceConfig(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_API_KEY", "bnb-key")
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_SECRET_KEY", "bnb-secret")
	cfg, err := LoadConfig(writeTempYAML(t, `
trading:
  symbol: "ETHUSDT"
  order_quantity: 30
  buy_window_size: 10
`))
	if err != nil {
		t.Fatal(err)
	}
	ex := cfg.Exchanges.Binance
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

func TestMaxMarginUsagePercentFromYAML(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{name: "default", want: 100},
		{name: "valid", value: "65.5", want: 65.5},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "over 100", value: "100.01", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := envTestYAML
			if tt.value != "" {
				body += "  max_margin_usage_percent: " + tt.value + "\n"
			}
			cfg, err := LoadConfig(writeTempYAML(t, body))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Trading.MaxMarginUsagePercent != tt.want {
				t.Fatalf("max margin usage percent = %v, want %v", cfg.Trading.MaxMarginUsagePercent, tt.want)
			}
		})
	}
}

func TestMaxMarginUsagePercentRejectsExplicitZeroFromYAMLMerge(t *testing.T) {
	body := `
trading_defaults: &trading_defaults
  max_margin_usage_percent: 0
exchanges:
  binance:
    api_key: "yaml-key"
    secret_key: "yaml-secret"
trading:
  <<: *trading_defaults
  symbol: "ETHUSDT"
  order_quantity: 30
  buy_window_size: 10
`
	if _, err := LoadConfig(writeTempYAML(t, body)); err == nil {
		t.Fatal("expected merged explicit zero to fail validation")
	}
}

func TestMaxMarginUsagePercentFromEnv(t *testing.T) {
	t.Setenv("OPENSQT_TRADING_MAX_MARGIN_USAGE_PERCENT", "72.5")
	cfg, err := LoadConfig(writeTempYAML(t, envTestYAML+"  max_margin_usage_percent: 80\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trading.MaxMarginUsagePercent != 72.5 {
		t.Fatalf("max margin usage percent = %v", cfg.Trading.MaxMarginUsagePercent)
	}
}

func TestInvalidMaxMarginUsagePercentFromEnv(t *testing.T) {
	for _, value := range []string{"0", "-1", "100.01", "NaN"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OPENSQT_TRADING_MAX_MARGIN_USAGE_PERCENT", value)
			if _, err := LoadConfig(writeTempYAML(t, envTestYAML)); err == nil {
				t.Fatalf("expected validation error for %q", value)
			}
		})
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

func TestLoadConfigEnvOnlyWithoutYAML(t *testing.T) {
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_API_KEY", "env-key")
	t.Setenv("OPENSQT_EXCHANGES_BINANCE_SECRET_KEY", "env-secret")
	t.Setenv("OPENSQT_TRADING_SYMBOL", "ETHUSDC")
	t.Setenv("OPENSQT_TRADING_ORDER_QUANTITY", "22")
	t.Setenv("OPENSQT_TRADING_BUY_WINDOW_SIZE", "10")

	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trading.Symbol != "ETHUSDC" {
		t.Fatalf("symbol = %s", cfg.Trading.Symbol)
	}
	ex := cfg.Exchanges.Binance
	if ex.APIKey != "env-key" || ex.SecretKey != "env-secret" {
		t.Fatalf("exchange = %+v", ex)
	}
	if cfg.Trading.OrderQuantity != 22 || cfg.Trading.BuyWindowSize != 10 {
		t.Fatalf("trading = %+v", cfg.Trading)
	}
}

func TestLoadConfigMissingYAMLWithoutEnvFails(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil {
		t.Fatal("expected validation error without yaml or env")
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
