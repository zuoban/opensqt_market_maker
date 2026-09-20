// Package fillledger is a persistence contract prototype. It is deliberately
// not wired into the trading manager; see FILL_LEDGER_DESIGN.md before integration.
package fillledger

import (
	"fmt"
	"strings"

	"opensqt/utils"
)

// Scope binds one ledger to a stable, non-secret account reference and strategy.
// AccountRef must not be an API key or a hash of one: key rotation is not a new account.
type Scope struct {
	Exchange         string `json:"exchange"`
	Environment      string `json:"environment"`
	AccountRef       string `json:"accountRef"`
	Symbol           string `json:"symbol"`
	StrategyRevision string `json:"strategyRevision"`
}

func (s Scope) validate() error {
	if s.Exchange != "binance" || (s.Environment != "mainnet" && s.Environment != "testnet") {
		return fmt.Errorf("%w: exchange/environment", ErrInvalid)
	}
	for _, value := range []string{s.AccountRef, s.Symbol, s.StrategyRevision} {
		if value == "" || len(value) > 80 {
			return fmt.Errorf("%w: scope field length", ErrInvalid)
		}
		for _, c := range value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return fmt.Errorf("%w: scope field characters", ErrInvalid)
			}
		}
	}
	if s.Symbol != strings.ToUpper(s.Symbol) {
		return fmt.Errorf("%w: symbol must be uppercase", ErrInvalid)
	}
	return nil
}

// Completion identifies a fully executed exchange order, not an individual trade.
// OrderID (scoped by account/environment/symbol) is the durable identity.
// ClientOrderID can be reused across process starts and is only corroborating data.
// Quantities use canonical decimal strings, never float serialization.
type Completion struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	Side          string `json:"side"`
	ExecutedQty   string `json:"executedQty"`
	ExecutedQuote string `json:"executedQuote"`
}

func (f Completion) canonical() (Completion, error) {
	if f.OrderID <= 0 || f.Side != "BUY" && f.Side != "SELL" {
		return Completion{}, fmt.Errorf("%w: order identity/side", ErrInvalid)
	}
	if len(f.ClientOrderID) > 36 {
		return Completion{}, fmt.Errorf("%w: client ID length", ErrInvalid)
	}
	for _, c := range f.ClientOrderID {
		if c < 33 || c > 126 {
			return Completion{}, fmt.Errorf("%w: client ID characters", ErrInvalid)
		}
	}
	f.ClientOrderID = utils.RemoveBinanceBrokerPrefix(f.ClientOrderID)
	var err error
	f.ExecutedQty, err = positiveDecimal(f.ExecutedQty)
	if err != nil {
		return Completion{}, err
	}
	f.ExecutedQuote, err = positiveDecimal(f.ExecutedQuote)
	return f, err
}

func positiveDecimal(value string) (string, error) {
	canonical, err := canonicalAmount(value, false)
	if err != nil {
		return "", err
	}
	if canonical == "0" {
		return "", fmt.Errorf("%w: execution must be positive", ErrInvalid)
	}
	return canonical, nil
}
