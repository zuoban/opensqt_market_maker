package config

import (
	"strings"
	"testing"
)

func TestDashboardDefaults(t *testing.T) {
	c := &Config{}
	c.Exchanges.Binance = BinanceConfig{APIKey: "k", SecretKey: "s"}
	c.Trading.Symbol = "ETHUSDT"
	c.Trading.OrderQuantity = 30
	c.Trading.BuyWindowSize = 2
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.DashboardEnabled() {
		t.Fatal("dashboard should default on")
	}
	if c.Dashboard.Listen != "127.0.0.1:8787" {
		t.Fatalf("listen = %s", c.Dashboard.Listen)
	}
	if c.Dashboard.PushIntervalMS != 400 {
		t.Fatalf("push = %d", c.Dashboard.PushIntervalMS)
	}
	if c.Trading.MaxMarginUsagePercent != 100 {
		t.Fatalf("max margin usage percent = %v", c.Trading.MaxMarginUsagePercent)
	}
	if c.Trading.OrderCleanupThreshold != 100 {
		t.Fatalf("cleanup threshold = %d, want default 100", c.Trading.OrderCleanupThreshold)
	}
	c.Dashboard.PushIntervalMS = 50
	if err := c.applyDashboardDefaults(); err != nil {
		t.Fatal(err)
	}
	if c.Dashboard.PushIntervalMS != 200 {
		t.Fatalf("min push = %d", c.Dashboard.PushIntervalMS)
	}
	off := false
	c.Dashboard.Enabled = &off
	if c.DashboardEnabled() {
		t.Fatal("explicit false should disable")
	}
}

func validTradingConfig() *Config {
	c := &Config{}
	c.Exchanges.Binance = BinanceConfig{APIKey: "k", SecretKey: "s"}
	c.Trading.Symbol = "ETHUSDT"
	c.Trading.OrderQuantity = 30
	c.Trading.BuyWindowSize = 10
	c.Trading.SellWindowSize = 10
	c.Trading.OrderCleanupThreshold = 50
	return c
}

func TestCleanupThresholdMustExceedCombinedWindows(t *testing.T) {
	c := validTradingConfig()
	c.Trading.OrderCleanupThreshold = 20
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error when buy_window + sell_window >= order_cleanup_threshold")
	}
	if !strings.Contains(err.Error(), "默认 100") || !strings.Contains(err.Error(), "50") {
		t.Fatalf("startup error should explain the default and old example threshold: %v", err)
	}

	c = validTradingConfig()
	c.Trading.OrderCleanupThreshold = 21
	if err := c.Validate(); err != nil {
		t.Fatalf("threshold just above combined windows should pass: %v", err)
	}
}
