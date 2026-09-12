package config

import (
	"math"
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
	if c.Execution.MakerGuardTicks != 2 || c.Execution.QuoteStaleMS != 30000 ||
		c.Execution.PostOnlyRetryMinMS != 50 || c.Execution.PostOnlyRetryMaxMS != 500 ||
		c.Execution.PostOnlyRetryBurst != 5 || c.Execution.CatchUpMode != "passive" ||
		c.Execution.MaxActiveCatchUpSlots != 1 || c.Execution.MaxCatchUpSlotsPerAdjust != 1 ||
		c.Execution.MaxCatchUpDistanceRatio != 0.5 || c.Execution.NearTouchSingleOrderRatio != 0.15 ||
		c.Execution.MaxGapStackSlots != 0 {
		t.Fatalf("execution defaults = %+v", c.Execution)
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

func TestExecutionConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		want      string
	}{
		{name: "negative guard", configure: func(c *Config) { c.Execution.MakerGuardTicks = -1 }, want: "maker_guard_ticks"},
		{name: "negative stale", configure: func(c *Config) { c.Execution.QuoteStaleMS = -1 }, want: "quote_stale_ms"},
		{name: "retry bounds", configure: func(c *Config) {
			c.Execution.PostOnlyRetryMinMS = 100
			c.Execution.PostOnlyRetryMaxMS = 50
		}, want: "post_only_retry_max_ms"},
		{name: "negative burst", configure: func(c *Config) { c.Execution.PostOnlyRetryBurst = -1 }, want: "post_only_retry_burst"},
		{name: "catch up mode", configure: func(c *Config) { c.Execution.CatchUpMode = "taker" }, want: "catch_up_mode"},
		{name: "active catch up", configure: func(c *Config) { c.Execution.MaxActiveCatchUpSlots = -1 }, want: "max_active_catch_up_slots"},
		{name: "per adjust", configure: func(c *Config) { c.Execution.MaxCatchUpSlotsPerAdjust = -1 }, want: "max_catch_up_slots_per_adjust"},
		{name: "catch up ratio", configure: func(c *Config) { c.Execution.MaxCatchUpDistanceRatio = 1.01 }, want: "max_catch_up_distance_ratio"},
		{name: "near touch ratio", configure: func(c *Config) { c.Execution.NearTouchSingleOrderRatio = math.NaN() }, want: "near_touch_single_order_ratio"},
		{name: "gap stack negative", configure: func(c *Config) { c.Execution.MaxGapStackSlots = -1 }, want: "max_gap_stack_slots"},
		{name: "gap stack too large", configure: func(c *Config) { c.Execution.MaxGapStackSlots = 11 }, want: "max_gap_stack_slots"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validTradingConfig()
			tt.configure(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want field %q", err, tt.want)
			}
		})
	}

	c := validTradingConfig()
	c.Execution.CatchUpMode = " PASSIVE "
	if err := c.Validate(); err != nil || c.Execution.CatchUpMode != "passive" {
		t.Fatalf("normalized catch-up mode=%q err=%v", c.Execution.CatchUpMode, err)
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
