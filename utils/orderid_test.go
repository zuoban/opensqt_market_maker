package utils

import "testing"

func TestParseOrderIDRejectsInvalidDirectionAndPrice(t *testing.T) {
	tests := []string{
		"100_X_1700000000001",
		"100_BUY_1700000000001",
		"0_B_1700000000001",
		"-100_S_1700000000001",
	}
	for _, orderID := range tests {
		t.Run(orderID, func(t *testing.T) {
			if price, side, timestamp, valid := ParseOrderID(orderID, 2); valid {
				t.Fatalf("ParseOrderID(%q) = (%v, %q, %d, true), want invalid", orderID, price, side, timestamp)
			}
		})
	}
}

func TestParseOrderIDAcceptsOnlyCanonicalDirections(t *testing.T) {
	tests := []struct {
		orderID  string
		wantSide string
	}{
		{orderID: "100_B_1700000000001", wantSide: "BUY"},
		{orderID: "100_S_1700000000001", wantSide: "SELL"},
	}
	for _, tt := range tests {
		t.Run(tt.wantSide, func(t *testing.T) {
			price, side, timestamp, valid := ParseOrderID(tt.orderID, 2)
			if !valid || price != 1 || side != tt.wantSide || timestamp != 1_700_000_000 {
				t.Fatalf("ParseOrderID(%q) = (%v, %q, %d, %v)", tt.orderID, price, side, timestamp, valid)
			}
		})
	}
}
