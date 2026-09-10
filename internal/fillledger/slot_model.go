package fillledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

const (
	slotFormatVersion   = 2
	MaxSlotStateBytes   = 64 << 10
	MaxTransactionSlots = 64
)

// SlotWrite contains only one affected slot. State is a versioned business DTO
// owned by the caller, not a serialized InventorySlot (which contains a mutex).
// Key is the canonical positive decimal logical grid price, never an order price.
type SlotWrite struct {
	Key   string `json:"key"`
	State []byte `json:"state"`
}

type SlotRecord struct {
	SlotWrite
	Revision uint64 `json:"revision"`
}

// OrderCheckpoint keeps one latest progress record per exchange OrderID. Raw
// exchange decimals must be supplied before conversion to floating point. State
// carries caller-owned terminal accounting / completion details, including PNL.
// Scope.StrategyRevision identifies the caller's state encoding and semantics.
type OrderCheckpoint struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	Side          string `json:"side"`
	SlotKey       string `json:"slotKey"`
	Status        string `json:"status"`
	ExecutedQty   string `json:"executedQty"`
	ExecutedQuote string `json:"executedQuote"`
	UpdateTime    int64  `json:"updateTime"`
	State         []byte `json:"state"`
}

type OrderRecord struct {
	OrderCheckpoint
	Revision       uint64   `json:"revision"`
	TransitionHash [32]byte `json:"transitionHash"`
}

// Accounting represents totals or transaction deltas. Quantities are unsigned;
// PNL may be negative. Decimal addition is exact, without float64 round trips.
// The caller still owns how an execution's PNL/cost is calculated.
type Accounting struct {
	BuyQty       string `json:"buyQty"`
	SellQty      string `json:"sellQty"`
	RealizedPNL  string `json:"realizedPnl"`
	FilledOrders uint64 `json:"filledOrders"`
}

func ZeroAccounting() Accounting { return Accounting{BuyQty: "0", SellQty: "0", RealizedPNL: "0"} }

type SlotTransaction struct {
	ExpectedRevision uint64
	Slots            []SlotWrite
	Order            OrderCheckpoint
	Delta            Accounting
}

// SlotView always contains CURRENT selected records and totals, including on a
// duplicate commit. It never contains the historical projection of that replay.
// Callers must not publish their submitted deltas again when Duplicate is true.
type SlotView struct {
	Revision uint64
	Totals   Accounting
	Slots    map[string]SlotRecord
	Orders   map[int64]OrderRecord
}

type SlotCommitResult struct {
	View      SlotView
	Duplicate bool
}

type slotHead struct {
	Revision   uint64     `json:"revision"`
	SlotCount  uint64     `json:"slotCount"`
	OrderCount uint64     `json:"orderCount"`
	Totals     Accounting `json:"totals"`
}

func canonicalAmount(value string, signed bool) (string, error) {
	if len(value) == 0 || len(value) > 80 {
		return "", fmt.Errorf("%w: amount length", ErrInvalid)
	}
	negative := strings.HasPrefix(value, "-")
	if negative {
		if !signed {
			return "", fmt.Errorf("%w: negative quantity", ErrInvalid)
		}
		value = value[1:]
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return "", ErrInvalid
	}
	for _, part := range parts {
		if part == "" {
			return "", ErrInvalid
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", ErrInvalid
			}
		}
	}
	whole := strings.TrimLeft(parts[0], "0")
	if whole == "" {
		whole = "0"
	}
	if len(parts) == 2 {
		if fraction := strings.TrimRight(parts[1], "0"); fraction != "" {
			whole += "." + fraction
		}
	}
	if negative && whole != "0" {
		whole = "-" + whole
	}
	return whole, nil
}

func amountRat(value string) *big.Rat {
	r, _ := new(big.Rat).SetString(value) // Internal callers only pass validated decimals.
	return r
}

func addAmount(a, b string) (string, error) {
	scale := func(s string) int {
		if i := strings.IndexByte(s, '.'); i >= 0 {
			return len(s) - i - 1
		}
		return 0
	}
	sum := new(big.Rat).Add(amountRat(a), amountRat(b))
	return canonicalAmount(sum.FloatString(max(scale(a), scale(b))), true)
}

func canonicalAccounting(a Accounting) (Accounting, error) {
	var err error
	if a.BuyQty, err = canonicalAmount(a.BuyQty, false); err != nil {
		return Accounting{}, err
	}
	if a.SellQty, err = canonicalAmount(a.SellQty, false); err != nil {
		return Accounting{}, err
	}
	if a.RealizedPNL, err = canonicalAmount(a.RealizedPNL, true); err != nil {
		return Accounting{}, err
	}
	return a, nil
}

func terminalStatus(s string) bool { return s == "CANCELED" || s == "EXPIRED" || s == "REJECTED" }

func canonicalOrder(o OrderCheckpoint) (OrderCheckpoint, error) {
	// Reuse the established exchange/client identity validation, allowing zero
	// cumulative execution for NEW/rejected orders in this format.
	identity, err := (Completion{OrderID: o.OrderID, ClientOrderID: o.ClientOrderID, Side: o.Side, ExecutedQty: "1", ExecutedQuote: "1"}).canonical()
	if err != nil || identity.ClientOrderID == "" {
		return OrderCheckpoint{}, ErrInvalid
	}
	o.ClientOrderID = identity.ClientOrderID
	if o.SlotKey, err = positiveDecimal(o.SlotKey); err != nil {
		return OrderCheckpoint{}, err
	}
	if o.ExecutedQty, err = canonicalAmount(o.ExecutedQty, false); err != nil {
		return OrderCheckpoint{}, err
	}
	if o.ExecutedQuote, err = canonicalAmount(o.ExecutedQuote, false); err != nil {
		return OrderCheckpoint{}, err
	}
	if (o.ExecutedQty == "0") != (o.ExecutedQuote == "0") || o.UpdateTime < 0 {
		return OrderCheckpoint{}, ErrInvalid
	}
	if o.Status != "NEW" && o.Status != "PARTIALLY_FILLED" && o.Status != "FILLED" && !terminalStatus(o.Status) {
		return OrderCheckpoint{}, ErrInvalid
	}
	if (o.Status == "FILLED" || o.Status == "PARTIALLY_FILLED") && o.ExecutedQty == "0" {
		return OrderCheckpoint{}, ErrInvalid
	}
	if len(o.State) == 0 || len(o.State) > MaxSlotStateBytes {
		return OrderCheckpoint{}, ErrInvalid
	}
	o.State = bytes.Clone(o.State)
	return o, nil
}

func canonicalSlots(slots []SlotWrite) ([]SlotWrite, error) {
	next := make([]SlotWrite, len(slots))
	for i, s := range slots {
		key, err := positiveDecimal(s.Key)
		if err != nil || len(s.State) == 0 || len(s.State) > MaxSlotStateBytes {
			return nil, ErrInvalid
		}
		next[i] = SlotWrite{Key: key, State: bytes.Clone(s.State)}
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Key < next[j].Key })
	for i := 1; i < len(next); i++ {
		if next[i-1].Key == next[i].Key {
			return nil, ErrInvalid
		}
	}
	return next, nil
}

func canonicalTransaction(in SlotTransaction) (SlotTransaction, [32]byte, error) {
	var hash [32]byte
	if len(in.Slots) == 0 || len(in.Slots) > MaxTransactionSlots {
		return SlotTransaction{}, hash, ErrInvalid
	}
	var err error
	if in.Slots, err = canonicalSlots(in.Slots); err != nil {
		return SlotTransaction{}, hash, err
	}
	if in.Order, err = canonicalOrder(in.Order); err != nil {
		return SlotTransaction{}, hash, err
	}
	if in.Delta, err = canonicalAccounting(in.Delta); err != nil {
		return SlotTransaction{}, hash, err
	}
	hasOrderSlot := false
	size := len(in.Order.State)
	for _, slot := range in.Slots {
		size += len(slot.State)
		hasOrderSlot = hasOrderSlot || slot.Key == in.Order.SlotKey
	}
	if !hasOrderSlot || size > MaxStateBytes || in.Delta.FilledOrders > 1 {
		return SlotTransaction{}, hash, ErrInvalid
	}
	// The version is a precondition, not part of the exact last-transition receipt.
	data, _ := json.Marshal(struct {
		Slots []SlotWrite
		Order OrderCheckpoint
		Delta Accounting
	}{in.Slots, in.Order, in.Delta})
	return in, sha256.Sum256(data), nil
}
