package position

import "testing"

type binanceNamedExchange struct{ stubEx }

func (binanceNamedExchange) GetName() string { return "Binance" }

func TestCanonicalClientOrderIDMatchesBinanceBrokerPrefix(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, binanceNamedExchange{}, 2, 3)
	plain := "10000_B_1700000000001"
	prefixed := "x-zdfVM8vY" + plain
	if got := spm.canonicalClientOrderID(prefixed); got != plain {
		t.Fatalf("canonicalClientOrderID(%q) = %q, want %q", prefixed, got, plain)
	}
	if got := spm.canonicalClientOrderID(plain); got != plain {
		t.Fatalf("canonicalClientOrderID(%q) = %q, want unchanged", plain, got)
	}
}
