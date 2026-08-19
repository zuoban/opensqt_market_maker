package web

import (
	"encoding/json"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestVisualFixture serves deterministic dashboard data for manual browser QA.
// It is skipped during normal test runs and never ships as a production route.
func TestVisualFixture(t *testing.T) {
	if os.Getenv("WEB_VISUAL") == "" {
		t.Skip("set WEB_VISUAL=1 to hold a deterministic dashboard fixture")
	}

	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := visualFixtureSnapshot()
	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(staticRoot))))
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, snapshot)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, readErr := staticFS.ReadFile("static/index.html")
		if readErr != nil {
			http.Error(w, "index not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:18789")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Log("visual fixture http://127.0.0.1:18789")
	select {
	case <-time.After(2 * time.Minute):
		_ = server.Close()
	}
}

func visualFixtureSnapshot() map[string]interface{} {
	now := time.Now().UTC().Truncate(time.Minute)
	candles := make([]map[string]interface{}, 0, 96)
	for i := 0; i < 96; i++ {
		wave := math.Sin(float64(i)/6)*10 + float64(i)*0.18
		open := 4210 + wave
		closePrice := open + math.Sin(float64(i)*1.7)*4
		candles = append(candles, map[string]interface{}{
			"time":     now.Add(time.Duration(i-95) * time.Minute).UnixMilli(),
			"open":     open,
			"high":     math.Max(open, closePrice) + 3.2,
			"low":      math.Min(open, closePrice) - 2.8,
			"close":    closePrice,
			"volume":   120 + i*3,
			"isClosed": i < 95,
		})
	}

	slots := []map[string]interface{}{
		{"price": 4244.0, "priceText": "4,244.00", "positionStatus": "FILLED", "positionQty": 0.014, "positionQtyText": "0.0140", "orderSide": "SELL", "orderStatus": "PLACED", "slotStatus": "OCCUPIED", "inSellWindow": true},
		{"price": 4238.0, "priceText": "4,238.00", "orderSide": "SELL", "orderStatus": "CONFIRMED", "slotStatus": "OPEN", "inSellWindow": true},
		{"price": 4232.0, "priceText": "4,232.00", "slotStatus": "ANCHOR"},
		{"price": 4226.0, "priceText": "4,226.00", "orderSide": "BUY", "orderStatus": "PLACED", "slotStatus": "OPEN", "inBuyWindow": true},
		{"price": 4220.0, "priceText": "4,220.00", "positionStatus": "FILLED", "positionQty": 0.011, "positionQtyText": "0.0110", "slotStatus": "OCCUPIED", "inBuyWindow": true},
		{"price": 4208.0, "priceText": "4,208.00", "slotStatus": "OUTSIDE"},
	}

	latest := candles[len(candles)-1]["close"]
	filledOrders := visualFilledOrders()
	return map[string]interface{}{
		"time":      time.Now(),
		"version":   "visual-fixture",
		"startedAt": time.Now().Add(-2*time.Hour - 18*time.Minute),
		"uptimeSec": 8280,
		"app": map[string]interface{}{
			"exchange": "bitget", "symbol": "ETHUSDT", "orderQuantity": 30,
		},
		"price": map[string]interface{}{
			"last": latest, "lastText": "4,222.02", "updatedAt": time.Now(), "ageMs": 80,
		},
		"kline": map[string]interface{}{
			"interval": "1m", "updatedAt": time.Now(), "historyReady": true, "degraded": false, "candles": candles,
		},
		"position": map[string]interface{}{
			"initialized": true, "symbol": "ETHUSDT", "baseAsset": "ETH", "lastPrice": latest,
			"gridPrice": 4232, "priceDecimals": 2, "quantityDecimals": 4, "priceInterval": 6,
			"orderQuantity": 30, "buyWindowSize": 2, "sellWindowSize": 2, "slots": slots,
			"filledSlotCount": 2, "positionQty": 0.025, "activeBuyOrders": 1, "activeSellOrders": 2,
			"totalBuyQty": 1.26, "totalSellQty": 1.19, "estimatedProfit": 7.14, "realizedPnl": 6.82,
			"filledOrders":     filledOrders,
			"filledOrderCount": int64(len(filledOrders)),
		},
		"risk": map[string]interface{}{
			"enabled": true, "triggered": false, "lastMsg": "监控正常 · 量价均在阈值内",
			"symbols": []interface{}{
				map[string]interface{}{
					"symbol": "ETHUSDT", "status": "正常", "abnormal": false,
					"currentPrice": 4222.02, "avgPrice": 4216.4, "priceDeviation": 0.13, "volumeRatio": 1.08,
				},
				map[string]interface{}{
					"symbol": "BTCUSDT", "status": "关注", "abnormal": false,
					"currentPrice": 67420.5, "avgPrice": 67110.0, "priceDeviation": 0.46, "volumeRatio": 1.62,
				},
			},
		},
		"account": map[string]interface{}{
			"quoteAsset": "USDT", "available": 1864.22, "margin": 2018.47, "initialMarginReady": true,
			"initialMargin": 2000.0, "marginChange": 18.47, "marginChangePct": 0.92, "unrealizedPnl": 11.65,
		},
		"logs": []map[string]interface{}{
			{"time": time.Now().Add(-18 * time.Minute), "level": "INFO", "message": "账户缓存已同步 · 可用 1,864.22 USDT"},
			{"time": time.Now().Add(-9 * time.Minute), "level": "INFO", "message": "K 线与网格执行图已连接实时行情"},
			{"time": time.Now().Add(-4 * time.Minute), "level": "WARN", "message": "BTCUSDT 量比升至 1.62 · 继续观察"},
			{"time": time.Now(), "level": "INFO", "message": "ETHUSDT 买单已确认 · 4,214.00 × 0.0071"},
		},
	}
}

func visualFilledOrders() []interface{} {
	buyPattern := []int{2, 1, 3, 0, 2, 4, 1, 2, 1}
	sellPattern := []int{1, 2, 1, 2, 3, 1, 2, 0, 2}
	orders := make([]interface{}, 0, 32)
	seq := 1
	now := time.Now()
	for i, hoursAgo := 0, 8; i < 9; i, hoursAgo = i+1, hoursAgo-1 {
		base := now.Add(-time.Duration(hoursAgo) * time.Hour)
		for n := 0; n < buyPattern[i]; n++ {
			orders = append(orders, map[string]interface{}{
				"filledAt":    base.Add(time.Duration(6+n*5) * time.Minute),
				"side":        "BUY",
				"price":       4210.0 + float64(hoursAgo),
				"quantity":    0.0071,
				"realizedPnl": 0,
				"orderId":     seq,
			})
			seq++
		}
		for n := 0; n < sellPattern[i]; n++ {
			orders = append(orders, map[string]interface{}{
				"filledAt":    base.Add(time.Duration(18+n*4) * time.Minute),
				"side":        "SELL",
				"price":       4230.0 + float64(hoursAgo),
				"quantity":    0.0068,
				"realizedPnl": 0.12 + float64(n)*0.03,
				"orderId":     seq,
			})
			seq++
		}
	}
	return orders
}

func TestVisualFixtureSnapshotJSON(t *testing.T) {
	if _, err := json.Marshal(visualFixtureSnapshot()); err != nil {
		t.Fatal(err)
	}
}
