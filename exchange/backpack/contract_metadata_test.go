package backpack

import "testing"

func TestPriceTickSizeDoesNotGuessFromPriceDecimals(t *testing.T) {
	adapter := &BackpackAdapter{priceDecimals: 2}
	if got := adapter.GetPriceTickSize(); got != 0 {
		t.Fatalf("GetPriceTickSize() = %v, want 0 without validated tickSize", got)
	}
}
