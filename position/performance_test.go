package position

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"opensqt/telemetry"
)

var benchmarkSnapshotSink PositionSnapshot
var benchmarkBytesSink []byte
var benchmarkStateSink telemetry.StateCounts

func benchmarkPositionManager(n, c int) (*SuperPositionManager, []*InventorySlot) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 1
	cfg.Trading.SellWindowSize = max(c, 1)
	cfg.Trading.OrderCleanupThreshold = max(c, 1) + 1
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 10000
	spm.lastMarketPrice.Store(10000.0)
	candidates := make([]*InventorySlot, 0, c)
	for i := 0; i < n; i++ {
		p := 11000 + float64(i)
		if i < c {
			p = 9999 - float64(i)
		}
		slot := spm.getOrCreateSlot(p)
		slot.PositionStatus = PositionStatusFilled
		slot.PositionQty = 0.01
		if i < c {
			candidates = append(candidates, slot)
		}
	}
	buy := spm.getOrCreateSlot(10000)
	buy.OrderSide = "BUY"
	buy.OrderStatus = OrderStatusConfirmed
	buy.OrderID = 999999
	buy.ClientOID = "audit-buy"
	buy.OrderPrice = 10000
	buy.SlotStatus = SlotStatusLocked
	return spm, candidates
}

// BenchmarkAdjustOrdersScale measures planning and mock result handling, excluding network.
// Each iteration restores the same candidate reservations to keep the workload stable.
func BenchmarkAdjustOrdersScale(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		for _, c := range []int{0, 10, 100} {
			b.Run(fmt.Sprintf("slots=%d/sells=%d", n, c), func(b *testing.B) {
				spm, candidates := benchmarkPositionManager(n, c)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, s := range candidates {
						spm.clearReservationLocked(s)
					}
					if err := spm.AdjustOrders(10000); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
func BenchmarkPositionSnapshot(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			spm, _ := benchmarkPositionManager(n, 0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkSnapshotSink = spm.Snapshot()
			}
		})
	}
}

func BenchmarkTelemetryPendingStateCounts(b *testing.B) {
	spm, _ := benchmarkPositionManager(5000, 0)
	since := time.Now().Add(-time.Second)
	spm.forEachSlot(func(_ float64, slot *InventorySlot) bool {
		slot.oppositePending.Store(&oppositeWaitState{side: "SELL", since: since})
		return true
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkStateSink = spm.TelemetryStateCounts()
	}
}
func BenchmarkPositionSnapshotJSON(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			spm, _ := benchmarkPositionManager(n, 0)
			snap := spm.Snapshot()
			data, _ := json.Marshal(snap)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkBytesSink, _ = json.Marshal(snap)
			}
			b.ReportMetric(float64(len(data)), "payload_B")
		})
	}
}

// 与面板协商的压缩等级一致；仅测 JSON 压缩，不含编码、网络和浏览器解压。
func BenchmarkPositionSnapshotDeflate(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			spm, _ := benchmarkPositionManager(n, 0)
			payload, err := json.Marshal(spm.Snapshot())
			if err != nil {
				b.Fatal(err)
			}
			var compressed bytes.Buffer
			writer, err := flate.NewWriter(&compressed, flate.BestSpeed)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				compressed.Reset()
				writer.Reset(&compressed)
				if _, err := writer.Write(payload); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(payload)), "payload_B")
			b.ReportMetric(float64(compressed.Len()), "deflate_B")
		})
	}
}
func BenchmarkSlotPriceIndexChurn(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("slots=%d", n), func(b *testing.B) {
			idx := &slotPriceIndex{}
			for i := 0; i < n; i++ {
				idx.insert(float64(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				idx.insert(-1)
				idx.remove(-1)
			}
		})
	}
}
