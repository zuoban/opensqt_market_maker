package fillledger

import (
	"context"
	"path/filepath"
	"runtime/pprof"
	"testing"
)

// Setup is excluded from benchmark time/allocations. CPU profiles include it;
// select the ledger_phase=open label to inspect only recovery validation.
func BenchmarkSlotLedgerOpenScale(b *testing.B) {
	path := filepath.Join(b.TempDir(), "open.db")
	s, err := CreateSlots(path, fixtureScope(), scaleInitialSlots())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	seedScaleHistory(b, s, 100000)
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	pprof.Do(context.Background(), pprof.Labels("ledger_phase", "open"), func(context.Context) {
		for range b.N {
			s, err := OpenSlots(path, fixtureScope())
			if err != nil {
				b.Fatal(err)
			}
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
