package fillledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func fixtureScope() Scope {
	return Scope{Exchange: "binance", Environment: "testnet", AccountRef: "fixture-account", Symbol: "ETHUSDT", StrategyRevision: "grid-fixture-v1"}
}

func fixtureFill(id int64) Completion {
	return Completion{OrderID: id, ClientOrderID: fmt.Sprintf("10000_B_1700000000%03d", id), Side: "BUY", ExecutedQty: "0.1", ExecutedQuote: "10"}
}

// A deliberately small model makes the crash assertions independent of the
// storage encoding. Production inventory needs the larger schema in the design.
type model struct {
	QuantityUnits int `json:"quantityUnits"`
	CostUnits     int `json:"costUnits"`
	FilledOrders  int `json:"filledOrders"`
}

func encodedModel(t testing.TB, m model) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertModel(t testing.TB, got Snapshot, count int) {
	t.Helper()
	var m model
	if err := json.Unmarshal(got.State, &m); err != nil {
		t.Fatal(err)
	}
	want := model{QuantityUnits: count * 100, CostUnits: count * 10000, FilledOrders: count}
	if m != want || got.Revision != uint64(count) {
		t.Fatalf("checkpoint=%+v revision=%d want %+v", m, got.Revision, want)
	}
}

func newFixture(t testing.TB) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fills.db")
	s, err := Create(path, fixtureScope(), encodedModel(t, model{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func nextModel(t testing.TB, count int) []byte {
	return encodedModel(t, model{QuantityUnits: count * 100, CostUnits: count * 10000, FilledOrders: count})
}

func TestCommitRestartAndDuplicateKeepOneInventoryEffect(t *testing.T) {
	s, path := newFixture(t)
	input := nextModel(t, 1)
	snapshot, duplicate, err := s.Commit(0, fixtureFill(1), input)
	if err != nil || duplicate {
		t.Fatalf("commit duplicate=%v err=%v", duplicate, err)
	}
	assertModel(t, snapshot, 1)
	input[0], snapshot.State[0] = 'X', 'X'
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snapshot, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertModel(t, snapshot, 1)
	replay := fixtureFill(1)
	replay.ClientOrderID = "x-zdfVM8vY" + replay.ClientOrderID
	replay.ExecutedQty, replay.ExecutedQuote = "00.1000", "010.000"
	// A stale revision on an identical replay is a duplicate, not a lost update.
	snapshot, duplicate, err = s.Commit(0, replay, nextModel(t, 2))
	if err != nil || !duplicate {
		t.Fatalf("replay duplicate=%v err=%v", duplicate, err)
	}
	assertModel(t, snapshot, 1)
	completion, found, err := s.Lookup(1)
	if err != nil || !found || completion != fixtureFill(1) {
		t.Fatalf("lookup=%+v found=%v err=%v", completion, found, err)
	}
	_, found, err = s.Lookup(2)
	if err != nil || found {
		t.Fatalf("missing lookup found=%v err=%v", found, err)
	}
}

func TestOldDuplicateReturnsCurrentCheckpoint(t *testing.T) {
	s, _ := newFixture(t)
	for i := 1; i <= 2; i++ {
		if _, _, err := s.Commit(uint64(i-1), fixtureFill(int64(i)), nextModel(t, i)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, duplicate, err := s.Commit(0, fixtureFill(1), nextModel(t, 1))
	if err != nil || !duplicate {
		t.Fatalf("duplicate=%v err=%v", duplicate, err)
	}
	assertModel(t, snapshot, 2) // Never restore the old fill's obsolete checkpoint.
}

func TestExchangeOrderIdentitySurvivesClientIDReuse(t *testing.T) {
	s, _ := newFixture(t)
	first := fixtureFill(1)
	if _, _, err := s.Commit(0, first, nextModel(t, 1)); err != nil {
		t.Fatal(err)
	}
	second := first
	second.OrderID = 2 // Simulate a restart generating the same local ID.
	snapshot, duplicate, err := s.Commit(1, second, nextModel(t, 2))
	if err != nil || duplicate {
		t.Fatalf("client reuse duplicate=%v err=%v", duplicate, err)
	}
	assertModel(t, snapshot, 2)
}

func TestConcurrentDuplicateCommitsHaveOneWinner(t *testing.T) {
	s, _ := newFixture(t)
	var wg sync.WaitGroup
	results := make(chan bool, 20)
	errs := make(chan error, 20)
	state := nextModel(t, 1)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, duplicate, err := s.Commit(0, fixtureFill(1), state)
			results <- duplicate
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	winners := 0
	for duplicate := range results {
		if !duplicate {
			winners++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("first commits=%d want 1", winners)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertModel(t, snapshot, 1)
}

func TestRevisionConflictDoesNotPersistStaleProjection(t *testing.T) {
	s, _ := newFixture(t)
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Commit(0, fixtureFill(2), nextModel(t, 2)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Health(); err != nil {
		t.Fatal(err)
	}
	_, found, err := s.Lookup(2)
	if err != nil || found {
		t.Fatalf("stale transaction created receipt: found=%v err=%v", found, err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertModel(t, snapshot, 1)
}

func TestConflictingExecutionLatchesUnavailable(t *testing.T) {
	s, path := newFixture(t)
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); err != nil {
		t.Fatal(err)
	}
	conflict := fixtureFill(1)
	conflict.ExecutedQty = "0.2"
	if _, _, err := s.Commit(1, conflict, nextModel(t, 2)); !errors.Is(err, ErrCompletionConflict) {
		t.Fatalf("err=%v", err)
	}
	if !errors.Is(s.Health(), ErrUnavailable) {
		t.Fatal("conflicting accounting remained healthy")
	}
	_ = s.Close()
	reopened, err := Open(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertModel(t, snapshot, 1)
}

func TestScopeMismatchCannotReuseHistory(t *testing.T) {
	s, path := newFixture(t)
	_ = s.Close()
	for _, field := range []string{"account", "environment", "symbol", "strategy"} {
		t.Run(field, func(t *testing.T) {
			scope := fixtureScope()
			switch field {
			case "account":
				scope.AccountRef = "another-account"
			case "environment":
				scope.Environment = "mainnet"
			case "symbol":
				scope.Symbol = "BTCUSDT"
			case "strategy":
				scope.StrategyRevision = "grid-fixture-v2"
			}
			if unexpected, err := Open(path, scope); !errors.Is(err, ErrScopeMismatch) {
				if unexpected != nil {
					unexpected.Close()
				}
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestMissingEmptyAndExistingFilesAreNeverReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Open(path, fixtureScope()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing open err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created missing file: %v", err)
	}
	for _, content := range [][]byte{nil, []byte("incomplete database")} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, fixtureScope()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("incomplete open err=%v", err)
		}
		if _, err := Create(path, fixtureScope(), nextModel(t, 0)); !errors.Is(err, os.ErrExist) {
			t.Fatalf("existing create err=%v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, after) {
			t.Fatal("incomplete file was overwritten")
		}
	}
}

func TestExclusiveOpenAndPrivateFile(t *testing.T) {
	_, path := newFixture(t)
	if other, err := Open(path, fixtureScope()); err == nil {
		other.Close()
		t.Fatal("second writer opened same ledger")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions=%v", info.Mode().Perm())
	}
}

func TestInvalidInputsNeverPersist(t *testing.T) {
	s, _ := newFixture(t)
	for _, value := range []string{"", "0", "-1", "NaN", "1e100000000", "1..2", ".1", "1.", " 1"} {
		fill := fixtureFill(1)
		fill.ExecutedQty = value
		if _, _, err := s.Commit(0, fill, nextModel(t, 1)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("decimal %q err=%v", value, err)
		}
	}
	fill := fixtureFill(1)
	fill.OrderID = 0
	if _, _, err := s.Commit(0, fill, nextModel(t, 1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing exchange ID err=%v", err)
	}
	if _, _, err := s.Commit(0, fixtureFill(1), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty state err=%v", err)
	}
	if _, _, err := s.Commit(0, fixtureFill(1), make([]byte, MaxStateBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize state err=%v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	assertModel(t, snapshot, 0)
}

func TestLargeHistoryIsIndexedAfterReopen(t *testing.T) {
	s, path := newFixture(t)
	// Bulk fixture creation only: avoid thousands of fsyncs while testing old
	// exact lookups beyond a would-be bounded cache. Real commits always sync.
	const count = 5000
	err := s.db.Update(func(tx *bolt.Tx) error {
		for i := 1; i <= count; i++ {
			if err := putSealed(tx.Bucket(completedBucket), orderKey(int64(i)), receipt{Completion: fixtureFill(int64(i)), Revision: uint64(i)}); err != nil {
				return err
			}
		}
		return putSealed(tx.Bucket(metaBucket), checkpointKey, Snapshot{Revision: count, State: nextModel(t, count)})
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []int64{1, 1024, 4096, count} {
		got, found, err := s.Lookup(id)
		if err != nil || !found || got.OrderID != id {
			t.Fatalf("old order %d found=%v err=%v", id, found, err)
		}
	}
	snapshot, duplicate, err := s.Commit(0, fixtureFill(1), nextModel(t, count+1))
	if err != nil || !duplicate {
		t.Fatalf("old replay duplicate=%v err=%v", duplicate, err)
	}
	assertModel(t, snapshot, count)
}
