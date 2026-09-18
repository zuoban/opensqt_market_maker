package position

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"opensqt/logger"
)

// This workload calls the production callback with generated identities, not
// pre-populated maps. It keeps one live price and periodically retires/recreates
// that slot, so retained history is distinguishable from active inventory.
// No exchange, config file, background planner or network connection is used.
type historyWorkload struct {
	spm           *SuperPositionManager
	terminal      bool
	baseTime      int64
	updates       int
	fillNotices   int
	adjustNotices int
	retirements   int
}

func newHistoryWorkload(terminal bool) *historyWorkload {
	cfg := testConfig()
	cfg.Trading.OrderQuantity = 25 // .25 units at price 100.
	if terminal {
		cfg.Trading.OrderQuantity = 50 // Terminal corrections remain below original order quantity.
	}
	w := &historyWorkload{
		spm:      NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3),
		terminal: terminal,
		baseTime: time.Now().UnixMilli(),
	}
	w.spm.getOrCreateSlot(100)
	w.spm.SetFilledOrderNotifier(func(FilledOrderRecord) { w.fillNotices++ })
	w.spm.SetAdjustmentNotifier(func(time.Duration) { w.adjustNotices++ })
	return w
}

func (w *historyWorkload) event(cycle int, side, status string, qty, pnl float64, revision int64) OrderUpdate {
	id := int64(2*cycle + 1)
	price := 100.0
	quantity := .25
	if w.terminal {
		quantity = .5
	}
	if side == "SELL" {
		id++
		price = 101
	}
	return OrderUpdate{
		OrderID: id, ClientOrderID: fmt.Sprintf("10000_%s_%d", side[:1], 1700000000000+id),
		Side: side, Status: status, Quantity: quantity, ExecutedQty: qty,
		Price: price, AvgPrice: price, RealizedPNL: pnl, UpdateTime: w.baseTime + revision,
	}
}

func (w *historyWorkload) send(update OrderUpdate) {
	w.spm.OnOrderUpdate(update)
	w.updates++
}

func historyTerminalStatus(cycle int) string {
	return [...]string{"CANCELED", "EXPIRED", "REJECTED"}[cycle%3]
}

func (w *historyWorkload) cycle(t *testing.T, cycle int) {
	if !w.terminal {
		for _, side := range []string{"BUY", "SELL"} {
			pnl := 0.0
			if side == "SELL" {
				pnl = .125
			}
			w.send(w.event(cycle, side, "PARTIALLY_FILLED", .125, pnl/2, 0))
			full := w.event(cycle, side, "FILLED", .25, pnl, 1)
			w.send(full)
			full.ClientOrderID = "x-zdfVM8vY" + full.ClientOrderID
			w.send(full) // Broker-prefixed duplicate must use the same dedup key.
		}
	} else {
		status := historyTerminalStatus(cycle)
		buy := w.event(cycle, "BUY", status, .125, 0, 0)
		w.send(buy)
		// A different live order already owns the slot when the old BUY's
		// cumulative execution is corrected. Its binding must survive.
		sell := w.event(cycle, "SELL", "NEW", 0, 0, 1)
		w.send(sell)
		corrected := w.event(cycle, "BUY", status, .25, 0, 2)
		w.send(corrected)
		w.send(corrected)
		w.send(buy)
		slot := w.spm.lockMappedSlot(100)
		bound := slot.ClientOID == sell.ClientOrderID && slot.OrderID == sell.OrderID &&
			slot.OrderSide == "SELL" && slot.OrderStatus == OrderStatusConfirmed &&
			slot.SlotStatus == SlotStatusLocked && slot.OrderFilledQty == 0
		slot.mu.Unlock()
		if !bound {
			t.Fatalf("cycle %d: old terminal correction replaced current order", cycle)
		}
		w.send(w.event(cycle, "SELL", status, .125, .125, 3))
		w.send(w.event(cycle, "SELL", status, .25, .25, 4))
		pnlCorrection := w.event(cycle, "SELL", status, .25, .125, 5)
		w.send(pnlCorrection)
		w.send(pnlCorrection)
		w.send(w.event(cycle, "SELL", status, .25, .25, 4))
	}
	if (cycle+1)%256 == 0 {
		slot := w.spm.lockMappedSlot(100)
		deleted := w.spm.deleteSlotIfCurrentAndRecyclable(100, slot)
		slot.mu.Unlock()
		if !deleted || !slot.retired.Load() || w.spm.getOrCreateSlot(100) == slot {
			t.Fatalf("cycle %d: empty slot was not retired and recreated", cycle)
		}
		w.retirements++
	}
}

func (w *historyWorkload) assertState(t *testing.T, cycles int) {
	t.Helper()
	assertClose(t, "history buy quantity", w.spm.GetTotalBuyQty(), float64(cycles)/4)
	assertClose(t, "history sell quantity", w.spm.GetTotalSellQty(), float64(cycles)/4)
	assertClose(t, "history PNL", w.spm.GetRealizedPNL(), float64(cycles)/8)
	slot := w.spm.lockMappedSlot(100)
	empty := slot.PositionQty == 0 && slot.PositionCost == 0 && slot.PositionStatus == PositionStatusEmpty &&
		slot.OrderID == 0 && slot.ClientOID == "" && slot.SlotStatus == SlotStatusFree
	slot.mu.Unlock()
	if !empty {
		t.Fatal("lifecycle did not return to empty unbound inventory")
	}
	fills, terminals, adjustments, events := 2*cycles, 0, 2*cycles, 6*cycles
	if w.terminal {
		fills, terminals, adjustments, events = 0, 2*cycles, 5*cycles, 10*cycles
	}
	if len(w.spm.filledOrderKeys) != fills || w.spm.filledOrderCount != int64(fills) ||
		len(w.spm.terminalOrders) != terminals || w.fillNotices != fills ||
		w.adjustNotices != adjustments || w.updates != events {
		t.Fatalf("history counts: fills=%d/%d terminals=%d notices=%d/%d events=%d; cycles=%d terminal=%v",
			len(w.spm.filledOrderKeys), w.spm.filledOrderCount, len(w.spm.terminalOrders),
			w.fillNotices, w.adjustNotices, w.updates, cycles, w.terminal)
	}
	if len(w.spm.slotIndex.snapshot()) != 1 || len(w.spm.resolvedAbsentOrders) != 0 ||
		len(w.spm.filledOrders) != min(fills, maxRecentFilledOrders) || len(w.spm.filledHourly) > hourlyFillHours ||
		w.retirements != cycles/256 {
		t.Fatal("bounded live state or retirement count changed")
	}
}

// Probe records far beyond the old 4096-entry history limit, with a new live
// binding in place. Exact binary fractions make accounting assertions stable
// even at millions of events; they do not rely on a growing epsilon.
func (w *historyWorkload) probeOldHistory(t *testing.T, cycles int) {
	t.Helper()
	current := w.event(cycles, "BUY", "NEW", 0, 0, 10)
	w.send(current)
	slot := w.spm.lockMappedSlot(100)
	before := readOrderSlotState(slot)
	slot.mu.Unlock()
	notices, adjustments := w.fillNotices, w.adjustNotices
	for _, cycle := range []int{0, cycles / 2, cycles - 1} {
		for _, side := range []string{"BUY", "SELL"} {
			status, pnl := "FILLED", 0.0
			if w.terminal {
				status = historyTerminalStatus(cycle)
			}
			if side == "SELL" {
				pnl = .125
			}
			full := w.event(cycle, side, status, .25, pnl, 5)
			full.ClientOrderID = "x-zdfVM8vY" + full.ClientOrderID
			w.send(full)
			w.send(w.event(cycle, side, "PARTIALLY_FILLED", .125, pnl/2, 0))
		}
	}
	slot = w.spm.lockMappedSlot(100)
	after := readOrderSlotState(slot)
	slot.mu.Unlock()
	if after != before || notices != w.fillNotices || adjustments != w.adjustNotices {
		t.Fatal("old replay changed the new binding/inventory or repeated a notification")
	}
	assertClose(t, "old replay PNL", w.spm.GetRealizedPNL(), float64(cycles)/8)
	assertClose(t, "old replay buys", w.spm.GetTotalBuyQty(), float64(cycles)/4)
	assertClose(t, "old replay sells", w.spm.GetTotalSellQty(), float64(cycles)/4)
	if w.terminal {
		// Correct the oldest records after every retirement and all history
		// growth. Apply only new execution and the downward PNL difference.
		for _, side := range []string{"BUY", "SELL"} {
			pnl := 0.0
			if side == "SELL" {
				pnl = .0625
			}
			correction := w.event(0, side, historyTerminalStatus(0), .375, pnl, 20)
			w.send(correction)
			w.send(correction)
			progress, found := w.spm.getTerminalOrderProgress(correction)
			if !found || progress.ExecutedQty != .375 || progress.AccountedPNL != pnl {
				t.Fatal("oldest terminal high-water mark was lost or applied twice")
			}
		}
		assertClose(t, "late buys", w.spm.GetTotalBuyQty(), float64(cycles)/4+.125)
		assertClose(t, "late sells", w.spm.GetTotalSellQty(), float64(cycles)/4+.125)
		assertClose(t, "late PNL", w.spm.GetRealizedPNL(), float64(cycles)/8-.0625)
		slot = w.spm.lockMappedSlot(100)
		after = readOrderSlotState(slot)
		slot.mu.Unlock()
		if after != before || w.fillNotices != notices || w.adjustNotices != adjustments+2 {
			t.Fatal("late corrections damaged the new binding or notification counts")
		}
	}
}

func quietHistoryTest(t *testing.T) {
	t.Helper()
	previous := logger.GetLevel()
	logger.SetLevel(logger.ERROR)
	t.Cleanup(func() { logger.SetLevel(previous) })
}

func TestOrderHistoryLifecycle(t *testing.T) {
	quietHistoryTest(t)
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%v", terminal), func(t *testing.T) {
			w := newHistoryWorkload(terminal)
			const cycles = 5000 // 10,000 unique orders, beyond historical eviction limits.
			for i := range cycles {
				w.cycle(t, i)
			}
			w.assertState(t, cycles)
			w.probeOldHistory(t, cycles)
		})
	}
}

type historyMeasurement struct {
	Workload        string  `json:"workload"`
	Orders          int     `json:"orders"`
	Updates         int     `json:"updates"`
	FilledKeys      int     `json:"filled_keys"`
	TerminalRecords int     `json:"terminal_records"`
	AbsentRecords   int     `json:"absent_records"`
	Slots           int     `json:"slots"`
	RecentFills     int     `json:"recent_fills"`
	HourlyBuckets   int     `json:"hourly_buckets"`
	HeapBytes       uint64  `json:"heap_bytes"`
	HeapGrowthBytes int64   `json:"heap_growth_bytes"`
	HeapObjects     uint64  `json:"heap_objects"`
	AllocatedBytes  uint64  `json:"allocated_bytes"`
	Mallocs         uint64  `json:"mallocs"`
	NaturalGCs      uint32  `json:"natural_gcs"`
	NaturalPauseMS  float64 `json:"natural_pause_ms"`
	ForcedGCMS      float64 `json:"forced_gc_ms"`
	WorkMS          float64 `json:"work_ms"`
}

// Opt-in to avoid million-order memory/CPU costs in ordinary tests. Run each
// subtest in its own go test process when comparing heap profiles, because Go
// allocation profiles and runtime memory counters are process-wide.
func TestOrderHistoryStress(t *testing.T) {
	orders := historyStressOrders(t)
	quietHistoryTest(t)
	t.Logf("environment: %s %s/%s GOMAXPROCS=%d; logger=ERROR telemetry=off; serial offline callbacks",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	for _, name := range []string{"filled", "terminal"} {
		t.Run(name, func(t *testing.T) {
			measureOrderHistory(t, name, orders)
		})
	}
}

func historyStressOrders(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("OPENSQT_HISTORY_STRESS_ORDERS")
	if raw == "" {
		t.Skip("set OPENSQT_HISTORY_STRESS_ORDERS to an even order count >= 10000")
	}
	orders, err := strconv.Atoi(raw)
	if err != nil || orders < 10000 || orders%2 != 0 || orders > 10000000 {
		t.Fatal("OPENSQT_HISTORY_STRESS_ORDERS must be even and in [10000, 10000000]")
	}
	return orders
}

func measureOrderHistory(t *testing.T, name string, orders int) {
	w := newHistoryWorkload(name == "terminal")
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	m := historyMeasurement{Workload: name}
	completed := 0
	for target := 10000; ; target = min(target*10, orders) {
		var start, end, live runtime.MemStats
		runtime.ReadMemStats(&start)
		started := time.Now()
		for cycle := completed / 2; cycle < target/2; cycle++ {
			w.cycle(t, cycle)
		}
		m.WorkMS += float64(time.Since(started)) / float64(time.Millisecond)
		runtime.ReadMemStats(&end)
		m.AllocatedBytes += end.TotalAlloc - start.TotalAlloc
		m.Mallocs += end.Mallocs - start.Mallocs
		m.NaturalGCs += end.NumGC - start.NumGC
		m.NaturalPauseMS += float64(end.PauseTotalNs-start.PauseTotalNs) / 1e6
		w.assertState(t, target/2)
		gcStarted := time.Now()
		runtime.GC()
		m.ForcedGCMS = float64(time.Since(gcStarted)) / float64(time.Millisecond)
		runtime.ReadMemStats(&live)
		m.Orders, m.Updates = target, w.updates
		m.FilledKeys, m.TerminalRecords = len(w.spm.filledOrderKeys), len(w.spm.terminalOrders)
		m.AbsentRecords, m.Slots = len(w.spm.resolvedAbsentOrders), len(w.spm.slotIndex.snapshot())
		m.RecentFills, m.HourlyBuckets = len(w.spm.filledOrders), len(w.spm.filledHourly)
		m.HeapBytes, m.HeapGrowthBytes, m.HeapObjects = live.HeapAlloc, int64(live.HeapAlloc)-int64(baseline.HeapAlloc), live.HeapObjects
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("history_measurement %s", data)
		completed = target
		if target == orders {
			break
		}
	}
	if dir := os.Getenv("OPENSQT_HISTORY_PROFILE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".heap.pprof")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writeErr := pprof.WriteHeapProfile(f)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("heap profile: write=%v close=%v", writeErr, closeErr)
		}
		t.Logf("heap/allocation profile: %s", path)
	}
	w.probeOldHistory(t, orders/2)
	runtime.KeepAlive(w)
}

func TestResolvedAbsentHistoryChurn(t *testing.T) {
	quietHistoryTest(t)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	runResolvedAbsentHistory(t, spm, 64000)
	// Insertion also prunes records older than grace + one minute, even when
	// neither a late event nor an explicit convergence check visited the key.
	spm.rememberResolvedAbsentOrder("expired", time.Now().Add(-2*time.Minute))
	spm.rememberResolvedAbsentOrder("current", time.Now().Add(time.Minute))
	if len(spm.resolvedAbsentOrders) != 1 {
		t.Fatal("insertion did not prune the expired grace record")
	}
	spm.forgetResolvedAbsentOrder("current")
	if len(spm.resolvedAbsentOrders) != 0 {
		t.Fatal("resolved-absent history did not drain")
	}
}

func runResolvedAbsentHistory(t *testing.T, spm *SuperPositionManager, orders int) {
	t.Helper()
	const window = 64
	// Expiry is checked with the existing explicit clock argument: no sleeps
	// and no production clock override. This is a bounded outstanding workload,
	// not a claim that the map has an absolute capacity under arbitrary bursts.
	for start := 0; start < orders; start += window {
		size := min(window, orders-start)
		deadline := time.Now().Add(time.Minute)
		for i := range size {
			spm.rememberResolvedAbsentOrder(fmt.Sprintf("absent-%d", start+i), deadline)
		}
		if len(spm.resolvedAbsentOrders) != size {
			t.Fatal("resolved-absent history retained earlier completed windows")
		}
		for i := range size {
			key := fmt.Sprintf("absent-%d", start+i)
			if i%2 == 0 {
				spm.forgetResolvedAbsentOrder(key)
			} else if converged, found := spm.resolvedAbsentOrderConverged(key, deadline); !converged || !found {
				t.Fatal("deadline did not release absent record")
			}
		}
	}
	if len(spm.resolvedAbsentOrders) != 0 {
		t.Fatal("resolved-absent history did not drain")
	}
}

func TestResolvedAbsentHistoryStress(t *testing.T) {
	orders := historyStressOrders(t)
	quietHistoryTest(t)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	runtime.GC()
	var start, end, live runtime.MemStats
	runtime.ReadMemStats(&start)
	started := time.Now()
	runResolvedAbsentHistory(t, spm, orders)
	workMS := float64(time.Since(started)) / float64(time.Millisecond)
	runtime.ReadMemStats(&end)
	gcStarted := time.Now()
	runtime.GC()
	gcMS := float64(time.Since(gcStarted)) / float64(time.Millisecond)
	runtime.ReadMemStats(&live)
	m := historyMeasurement{
		Workload: "resolved_absent_window_64", Orders: orders,
		AbsentRecords: len(spm.resolvedAbsentOrders), HeapBytes: live.HeapAlloc,
		HeapGrowthBytes: int64(live.HeapAlloc) - int64(start.HeapAlloc), HeapObjects: live.HeapObjects,
		AllocatedBytes: end.TotalAlloc - start.TotalAlloc, Mallocs: end.Mallocs - start.Mallocs,
		NaturalGCs: end.NumGC - start.NumGC, NaturalPauseMS: float64(end.PauseTotalNs-start.PauseTotalNs) / 1e6,
		ForcedGCMS: gcMS, WorkMS: workMS,
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("history_measurement %s", data)
	runtime.KeepAlive(spm)
}
