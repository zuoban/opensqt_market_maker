package config

import "testing"

func TestDashboardDefaults(t *testing.T) {
	c := &Config{}
	c.App.CurrentExchange = "bitget"
	c.Exchanges = map[string]ExchangeConfig{
		"bitget": {APIKey: "k", SecretKey: "s"},
	}
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
