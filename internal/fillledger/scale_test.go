package fillledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const scaleSlots = 1000

// Opaque, fixed-size storage DTOs, not the complete production trading schema.
var scaleSlotDTO = bytes.Repeat([]byte("s"), 1024)
var scaleOrderDTO = bytes.Repeat([]byte("o"), 256)

func scaleInitialSlots() []SlotWrite {
	slots := make([]SlotWrite, scaleSlots)
	for i := range slots {
		slots[i] = SlotWrite{Key: strconv.Itoa(100 + i), State: scaleSlotDTO}
	}
	return slots
}

func scaleTransaction(id int64) SlotTransaction {
	key := strconv.FormatInt(100+(id-1)/2%scaleSlots, 10)
	side, delta := "BUY", ZeroAccounting()
	delta.BuyQty = "1"
	if id%2 == 0 {
		side = "SELL"
		delta.BuyQty, delta.SellQty, delta.RealizedPNL = "0", "1", "0.125"
	}
	status := "FILLED"
	if (id-1)%4 < 2 {
		delta.FilledOrders = 1
	} else {
		status = [...]string{"CANCELED", "EXPIRED", "REJECTED"}[(id-3)/4%3]
	}
	return SlotTransaction{
		ExpectedRevision: uint64(id - 1), Slots: []SlotWrite{{Key: key, State: scaleSlotDTO}}, Delta: delta,
		Order: OrderCheckpoint{OrderID: id, ClientOrderID: fmt.Sprintf("10000_%s_%013d", side[:1], id),
			Side: side, SlotKey: key, Status: status, ExecutedQty: "1", ExecutedQuote: "100", UpdateTime: 100, State: scaleOrderDTO},
	}
}

func scaleTotals(count int64) Accounting {
	// Exact eighths, expressed as canonical decimals without float conversion.
	milliPNL := count / 2 * 125
	pnl, err := canonicalAmount(fmt.Sprintf("%d.%03d", milliPNL/1000, milliPNL%1000), true)
	if err != nil {
		panic(err)
	}
	return Accounting{BuyQty: strconv.FormatInt((count+1)/2, 10), SellQty: strconv.FormatInt(count/2, 10),
		RealizedPNL: pnl, FilledOrders: uint64(count/4*2 + min(count%4, 2))}
}

// Test-only bulk setup, outside latency measurements. Each bounded batch still
// syncs. It creates the same canonical rows/hashes/head as sequential Commit,
// but is NOT a production import API or evidence of per-order sync throughput.
func seedScaleHistory(t testing.TB, s *SlotStore, count int) {
	t.Helper()
	if s.store.db.(*bolt.DB).NoSync {
		t.Fatal("scale fixture must preserve synchronous writes")
	}
	for start := 1; start <= count; start += 1000 {
		end := min(start+999, count)
		if err := s.store.db.Update(func(tx *bolt.Tx) error {
			for id := start; id <= end; id++ {
				in, hash, err := canonicalTransaction(scaleTransaction(int64(id)))
				if err != nil {
					return err
				}
				if err := putSealed(tx.Bucket(progressBucket), orderKey(int64(id)), OrderRecord{OrderCheckpoint: in.Order, Revision: uint64(id), TransitionHash: hash}); err != nil {
					return err
				}
				if err := putSealed(tx.Bucket(slotBucket), []byte(in.Slots[0].Key), SlotRecord{SlotWrite: in.Slots[0], Revision: uint64(id)}); err != nil {
					return err
				}
			}
			return putSealed(tx.Bucket(metaBucket), slotHeadKey, slotHead{Revision: uint64(end), SlotCount: scaleSlots, OrderCount: uint64(end), Totals: scaleTotals(int64(end))})
		}); err != nil {
			t.Fatal(err)
		}
		if end%100000 == 0 {
			t.Logf("seeded %d orders (setup only)", end)
		}
	}
}

func TestSlotScaleFixtureMatchesPublicCommits(t *testing.T) {
	const count = 32
	var views [2]SlotView
	for i := range views {
		path := filepath.Join(t.TempDir(), "fixture.db")
		s, err := CreateSlots(path, fixtureScope(), scaleInitialSlots())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if i == 0 {
			seedScaleHistory(t, s, count)
		} else {
			for id := 1; id <= count; id++ {
				if _, err := s.Commit(scaleTransaction(int64(id))); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = OpenSlots(path, fixtureScope())
		if err != nil {
			t.Fatal(err)
		}
		ids, keys := make([]int64, count), make([]string, count)
		for j := range ids {
			ids[j], keys[j] = int64(j+1), strconv.Itoa(100+j)
		}
		views[i], err = s.Read(keys, ids)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(views[0], views[1]) {
		t.Fatal("bulk fixture differs from syncing public Commit")
	}
}

type scaleLatency struct {
	Samples int     `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
}

func summarizeScaleLatency(samples []time.Duration) scaleLatency {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	percentile := func(p int) float64 { return float64(samples[(len(samples)*p+99)/100-1]) / float64(time.Millisecond) }
	return scaleLatency{len(samples), percentile(50), percentile(95), percentile(99), percentile(100)}
}

type scaleProbe struct {
	OpenMS          float64      `json:"open_ms"`
	RestoreSlotsMS  float64      `json:"restore_known_slots_ms"`
	StartupAlloc    uint64       `json:"startup_allocated_bytes"`
	StartupGCs      uint32       `json:"startup_gcs"`
	HeapAfterGC     uint64       `json:"heap_after_gc_bytes"`
	HeapGrowth      int64        `json:"heap_growth_bytes"`
	SampledPeakHeap uint64       `json:"sampled_peak_heap_bytes"`
	Hit             scaleLatency `json:"random_hit"`
	Miss            scaleLatency `json:"random_miss"`
	CorrectionsOK   bool         `json:"old_corrections_and_replays_ok"`
}

type scaleReport struct {
	SeedOrders      int              `json:"seed_orders"`
	FinalOrders     int              `json:"final_orders"`
	Slots           int              `json:"slots"`
	SeedMS          float64          `json:"seed_ms"`
	FileSeedBytes   int64            `json:"file_seed_bytes"`
	FileFinalBytes  int64            `json:"file_final_bytes"`
	LogicalDBBytes  int64            `json:"logical_db_bytes"`
	CommitAlloc     uint64           `json:"commit_allocated_bytes"`
	CommitGCs       uint32           `json:"commit_gcs"`
	Commit          scaleLatency     `json:"sync_commit"`
	DuplicateCommit scaleLatency     `json:"exact_duplicate_commit"`
	CommitPhases    scaleWritePhases `json:"sync_commit_phases"`
	DuplicatePhases scaleWritePhases `json:"duplicate_commit_phases"`
	CommitSamplesMS []float64        `json:"sync_commit_samples_ms,omitempty"`
	Recovery        scaleProbe       `json:"recovery"`
}

// bbolt's write time covers dirty-page writes, their sync and the meta write/sync.
// It is not a pure fsync timer. Stats are sampled outside the timed loops.
type scaleWritePhases struct {
	TotalMS float64 `json:"total_ms"`
	WriteMS float64 `json:"write_and_sync_ms"`
	SpillMS float64 `json:"spill_ms"`
}

func measureScaleWritePhases(before, after bolt.Stats, samples []time.Duration) scaleWritePhases {
	delta := after.Sub(&before)
	var elapsed time.Duration
	for _, sample := range samples {
		elapsed += sample
	}
	return scaleWritePhases{float64(elapsed) / float64(time.Millisecond),
		float64(delta.TxStats.GetWriteTime()) / float64(time.Millisecond),
		float64(delta.TxStats.GetSpillTime()) / float64(time.Millisecond)}
}

func scaleEnvInt(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minimum || n > maximum {
		t.Fatalf("%s must be in [%d, %d]", name, minimum, maximum)
	}
	return n
}

func scaleFileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestSlotLedgerScale(t *testing.T) {
	maximum := scaleEnvInt(t, "OPENSQT_LEDGER_SCALE_ORDERS", 0, 1000, 1000000)
	if maximum == 0 {
		t.Skip("opt in with OPENSQT_LEDGER_SCALE_ORDERS")
	}
	samples := scaleEnvInt(t, "OPENSQT_LEDGER_SCALE_SAMPLES", 1000, 100, 5000)
	t.Logf("environment: %s %s/%s GOMAXPROCS=%d; slot DTO=1024 bytes order DTO=256 bytes; NoSync=false; OS page cache not evicted", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	for count := 1000; ; count = min(count*10, maximum) {
		t.Run(strconv.Itoa(count), func(t *testing.T) { runSlotLedgerScale(t, count, samples) })
		if count == maximum {
			break
		}
	}
}

func runSlotLedgerScale(t *testing.T, count, samples int) {
	path := filepath.Join(t.TempDir(), "scale.db")
	s, err := CreateSlots(path, fixtureScope(), scaleInitialSlots())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := scaleReport{SeedOrders: count, FinalOrders: count + samples, Slots: scaleSlots}
	started := time.Now()
	seedScaleHistory(t, s, count)
	r.SeedMS = float64(time.Since(started)) / float64(time.Millisecond)
	r.FileSeedBytes = scaleFileSize(t, path)
	commits, duplicates := make([]time.Duration, samples), make([]time.Duration, samples)
	runtime.GC()
	var before, after runtime.MemStats
	db := s.store.db.(*bolt.DB)
	statsBefore := db.Stats()
	runtime.ReadMemStats(&before)
	for i := range samples {
		in := scaleTransaction(int64(count + i + 1))
		started = time.Now()
		result, err := s.Commit(in)
		commits[i] = time.Since(started)
		if err != nil || result.Duplicate || result.View.Revision != uint64(count+i+1) || result.View.Totals != scaleTotals(int64(count+i+1)) {
			t.Fatalf("new commit %d: duplicate=%v revision=%d err=%v", i, result.Duplicate, result.View.Revision, err)
		}
	}
	runtime.ReadMemStats(&after)
	r.CommitAlloc, r.CommitGCs = after.TotalAlloc-before.TotalAlloc, after.NumGC-before.NumGC
	r.CommitPhases = measureScaleWritePhases(statsBefore, db.Stats(), commits)
	// These call Commit's exact-retry path, which now rolls back without writes.
	// It still differs from the business reducer's ignored replay path (which can
	// stop after Read). Keep the timings separate.
	statsBefore = db.Stats()
	for i := range samples {
		in := scaleTransaction(int64(count + i + 1))
		started = time.Now()
		result, err := s.Commit(in)
		duplicates[i] = time.Since(started)
		if err != nil || !result.Duplicate || result.View.Revision != uint64(count+samples) || result.View.Totals != scaleTotals(int64(count+samples)) {
			t.Fatalf("duplicate %d changed current state: %+v %v", i, result.View.Totals, err)
		}
	}
	r.DuplicatePhases = measureScaleWritePhases(statsBefore, db.Stats(), duplicates)
	// Optional chronological samples for the deployment script's queue estimate.
	// Copy before percentile summarization sorts the durations in place.
	if os.Getenv("OPENSQT_LEDGER_EXPORT_SAMPLES") == "1" {
		r.CommitSamplesMS = make([]float64, len(commits))
		for i, d := range commits {
			r.CommitSamplesMS[i] = float64(d) / float64(time.Millisecond)
		}
	}
	r.Commit, r.DuplicateCommit = summarizeScaleLatency(commits), summarizeScaleLatency(duplicates)
	if err := s.store.db.View(func(tx *bolt.Tx) error { r.LogicalDBBytes = tx.Size(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d history: sync P95=%.3fms P99=%.3fms; measuring fresh-process open", count, r.Commit.P95MS, r.Commit.P99MS)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSlotLedgerScaleProbe$", "-test.v")
	cmd.Env = append(os.Environ(), "OPENSQT_TEST_LEDGER_SCALE_PATH="+path, "OPENSQT_TEST_LEDGER_SCALE_COUNT="+strconv.Itoa(count+samples), "OPENSQT_LEDGER_SCALE_SAMPLES="+strconv.Itoa(samples))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery probe: %v\n%s", err, output)
	}
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		if _, data, ok := strings.Cut(line, "ledger_scale_probe "); ok {
			if err := json.Unmarshal([]byte(data), &r.Recovery); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found || !r.Recovery.CorrectionsOK {
		t.Fatalf("missing recovery evidence: %s", output)
	}
	r.FileFinalBytes = scaleFileSize(t, path)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger_scale %s", data)
}

// Sample Go heap only, not RSS or mmap-resident pages. Sampling may miss peaks.
func sampleScaleHeap() func() uint64 {
	stop, done := make(chan struct{}), make(chan uint64, 1)
	go func() {
		samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		var peak uint64
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			metrics.Read(samples)
			peak = max(peak, samples[0].Value.Uint64())
			select {
			case <-stop:
				done <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	return func() uint64 { close(stop); return <-done }
}

func TestSlotLedgerScaleProbe(t *testing.T) {
	path := os.Getenv("OPENSQT_TEST_LEDGER_SCALE_PATH")
	if path == "" {
		t.Skip("fresh-process helper")
	}
	count := scaleEnvInt(t, "OPENSQT_TEST_LEDGER_SCALE_COUNT", 0, 1, 1010000)
	if count == 0 {
		t.Fatal("fresh-process helper requires an expected order count")
	}
	samples := scaleEnvInt(t, "OPENSQT_LEDGER_SCALE_SAMPLES", 1000, 100, 5000)
	runtime.GC()
	var before, after, live runtime.MemStats
	runtime.ReadMemStats(&before)
	stopSampling := sampleScaleHeap()
	started := time.Now()
	s, err := OpenSlots(path, fixtureScope())
	r := scaleProbe{OpenMS: float64(time.Since(started)) / float64(time.Millisecond)}
	if err != nil {
		stopSampling()
		t.Fatal(err)
	}
	defer s.Close()
	started = time.Now()
	// The fixture knows its slot keys; production enumeration/migration is not
	// implemented. Reads remain bounded to the public maximum of 64 rows.
	for start := 0; start < scaleSlots; start += MaxTransactionSlots {
		keys := make([]string, min(MaxTransactionSlots, scaleSlots-start))
		for i := range keys {
			keys[i] = strconv.Itoa(100 + start + i)
		}
		view, err := s.Read(keys, nil)
		if err != nil || len(view.Slots) != len(keys) || view.Revision != uint64(count) || view.Totals != scaleTotals(int64(count)) {
			t.Fatalf("recovered slots/totals incorrect: %v", err)
		}
	}
	r.RestoreSlotsMS = float64(time.Since(started)) / float64(time.Millisecond)
	r.SampledPeakHeap = stopSampling()
	runtime.ReadMemStats(&after)
	r.StartupAlloc, r.StartupGCs = after.TotalAlloc-before.TotalAlloc, after.NumGC-before.NumGC
	runtime.GC()
	runtime.ReadMemStats(&live)
	r.HeapAfterGC, r.HeapGrowth = live.HeapAlloc, int64(live.HeapAlloc)-int64(before.HeapAlloc)
	for _, missing := range []bool{false, true} {
		times := make([]time.Duration, samples)
		state := uint64(42)
		for i := range samples {
			state = state*6364136223846793005 + 1
			id := int64(state%uint64(count)) + 1
			in := scaleTransaction(id)
			if missing {
				id += int64(count)
			}
			started = time.Now()
			view, err := s.Read([]string{in.Order.SlotKey}, []int64{id})
			times[i] = time.Since(started)
			order, found := view.Orders[id]
			if err != nil || found == missing || !missing && !reflect.DeepEqual(order.OrderCheckpoint, in.Order) || len(view.Slots) != 1 || view.Revision != uint64(count) {
				t.Fatalf("random read %d missing=%v failed: %v", id, missing, err)
			}
		}
		if missing {
			r.Miss = summarizeScaleLatency(times)
		} else {
			r.Hit = summarizeScaleLatency(times)
		}
	}
	verifyScaleOldCorrections(t, s, count)
	r.CorrectionsOK = true
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger_scale_probe %s", data)
	runtime.KeepAlive(s)
}

func verifyScaleOldCorrections(t *testing.T, s *SlotStore, count int) {
	t.Helper()
	want, revision := scaleTotals(int64(count)), uint64(count)
	for _, id := range []int64{3, 4, 4} {
		in := scaleTransaction(id)
		in.ExpectedRevision = revision
		in.Order.ExecutedQty, in.Order.ExecutedQuote, in.Order.UpdateTime = "1.5", "150", int64(revision+1)
		in.Order.State = []byte(fmt.Sprintf("corrected-%d", revision))
		in.Delta = ZeroAccounting()
		if id == 3 {
			in.Delta.BuyQty = "0.5"
			want.BuyQty, _ = addAmount(want.BuyQty, "0.5")
		} else if revision == uint64(count+1) {
			in.Delta.SellQty, in.Delta.RealizedPNL = "0.5", "0.0625"
			want.SellQty, _ = addAmount(want.SellQty, "0.5")
			want.RealizedPNL, _ = addAmount(want.RealizedPNL, "0.0625")
		} else {
			in.Delta.RealizedPNL = "-0.03125"
			want.RealizedPNL, _ = addAmount(want.RealizedPNL, "-0.03125")
		}
		result, err := s.Commit(in)
		if err != nil || result.Duplicate || result.View.Totals != want || result.View.Revision != revision+1 {
			t.Fatalf("old terminal correction failed: %+v %v", result, err)
		}
		revision++
		result, err = s.Commit(in)
		if err != nil || !result.Duplicate || result.View.Totals != want || result.View.Revision != revision {
			t.Fatal("old terminal correction repeated accounting")
		}
	}
	result, err := s.Commit(scaleTransaction(1))
	if err != nil || !result.Duplicate || result.View.Totals != want || result.View.Revision != revision {
		t.Fatal("old completed replay failed after corrections")
	}
}
