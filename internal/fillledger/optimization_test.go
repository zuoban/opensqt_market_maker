package fillledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"syscall"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestSlotReplayDoesNotWriteDatabase(t *testing.T) {
	s, _ := newSlotFixture(t)
	for id := int64(1); id <= 2; id++ {
		in := slotTransaction(id)
		in.ExpectedRevision = uint64(id - 1)
		if _, err := s.Commit(in); err != nil {
			t.Fatal(err)
		}
	}
	db := s.store.db.(*bolt.DB)
	before := db.Stats()
	var txID int
	if err := db.View(func(tx *bolt.Tx) error { txID = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := s.Commit(slotTransaction(1))
	if err != nil || !result.Duplicate || result.View.Revision != 2 || result.View.Totals.BuyQty != "0.08" {
		t.Fatalf("old retry lost current state: %+v %v", result, err)
	}
	after := db.Stats()
	delta := after.Sub(&before)
	if delta.TxStats.GetWrite() != 0 || delta.TxStats.GetWriteTime() != 0 {
		t.Fatal("exact retry performed a disk write")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.ID() != txID {
			t.Fatal("exact retry advanced the database transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Returned byte slices remain independent even though the read/write
	// transaction was rolled back rather than committed.
	result.View.Slots["100"].State[0] = 'X'
	result.View.Orders[1].State[0] = 'Y'
	view := mustSlotRead(t, s)
	if string(view.Slots["100"].State) != "partial" || string(view.Orders[1].State) != "partial-progress" {
		t.Fatal("replay result aliases durable records")
	}
}

type replayRollbackFault struct{ database }

func (f replayRollbackFault) Update(fn func(*bolt.Tx) error) error {
	err := f.database.Update(fn)
	return errors.Join(err, syscall.EIO)
}

func TestSlotReplayCannotHideTransactionFailure(t *testing.T) {
	s, _ := newSlotFixture(t)
	if _, err := s.Commit(slotTransaction(1)); err != nil {
		t.Fatal(err)
	}
	s.store.db = replayRollbackFault{s.store.db}
	result, err := s.Commit(slotTransaction(1))
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrUncertain) || result.Duplicate || result.View.Slots != nil {
		t.Fatalf("replay swallowed failure: %+v %v", result, err)
	}
	if !errors.Is(s.Health(), ErrUnavailable) {
		t.Fatal("transaction failure did not latch")
	}
}

func sealedTestPayload(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sealed{Payload: payload, SHA256: sha256.Sum256(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestReusedDecoderDoesNotInheritMissingFields(t *testing.T) {
	record := OrderRecord{OrderCheckpoint: slotTransaction(1).Order, Revision: 1, TransitionHash: [32]byte{1}}
	valid := sealedTestPayload(t, record)
	badRows := map[string][]byte{
		"empty-envelope": []byte(`{}`), "missing-payload": []byte(`{"sha256":[]}`),
		"null-record": sealedTestPayload(t, nil), "empty-record": sealedTestPayload(t, struct{}{}),
	}
	var fields map[string]json.RawMessage
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"orderId", "clientOrderId", "side", "slotKey", "status", "executedQty", "executedQuote", "state", "revision", "transitionHash"} {
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, field)
		badRows["missing-"+field] = sealedTestPayload(t, fields)
	}
	for _, field := range []string{"payload", "sha256"} {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(valid, &envelope); err != nil {
			t.Fatal(err)
		}
		delete(envelope, field)
		badRows["envelope-missing-"+field], err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, bad := range badRows {
		t.Run(name, func(t *testing.T) {
			var decoder sealedDecoder
			var reused OrderRecord
			if err := decoder.readOrderRecord(orderKey(1), valid, 1, &reused); err != nil {
				t.Fatal(err)
			}
			if err := decoder.readOrderRecord(orderKey(1), bad, 1, &reused); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("bad row inherited previous state: %v", err)
			}
		})
	}
}

func TestReusedDecoderRecordsOwnTheirData(t *testing.T) {
	first := OrderRecord{OrderCheckpoint: slotTransaction(1).Order, Revision: 1, TransitionHash: [32]byte{1}}
	second := first
	second.OrderID, second.State = 2, bytes.Repeat([]byte("z"), 4096)
	var decoder sealedDecoder
	a, err := decoder.orderRecord(orderKey(1), sealedTestPayload(t, first), 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := decoder.orderRecord(orderKey(2), sealedTestPayload(t, second), 1)
	if err != nil {
		t.Fatal(err)
	}
	clear(decoder.envelope.Payload)
	b.State[0] = 'X'
	if !equalOrderCheckpoint(a.OrderCheckpoint, first.OrderCheckpoint) {
		t.Fatal("decoder reuse changed an already returned order")
	}
}

var decimalSyntax = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var canonicalSyntax = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]*[1-9])?$`)

// The independent grammar/rational oracle protects exact decimal semantics
// across normalization changes, including zero, signs and boundary lengths.
func FuzzLedgerDecimalNormalization(f *testing.F) {
	for _, s := range []string{"0", "-0", "00.000", "0012.3400", "-000.0120", "0.0001", "-1.5", "1.2.3", "1.", ".1", "+1", "1e2", " 1", "١", "", strings.Repeat("9", 80), strings.Repeat("9", 81)} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, input string, signed bool) {
		valid := len(input) > 0 && len(input) <= 80 && decimalSyntax.MatchString(input) && (signed || input[0] != '-')
		got, err := canonicalAmount(input, signed)
		if (err == nil) != valid {
			t.Fatalf("input=%q signed=%v accepted=%v expected=%v", input, signed, err == nil, valid)
		}
		if !valid {
			return
		}
		// Force decimal syntax for integer inputs too: big.Rat otherwise accepts
		// leading-zero octal integers, which are outside the ledger's grammar.
		oracle := input
		if !strings.ContainsRune(oracle, '.') {
			oracle += ".0"
		}
		wantRat, _ := new(big.Rat).SetString(oracle)
		gotRat, ok := new(big.Rat).SetString(got)
		if !ok || !canonicalSyntax.MatchString(got) || got == "-0" || wantRat.Cmp(gotRat) != 0 {
			t.Fatalf("normalization changed value/format: %q -> %q", input, got)
		}
		again, err := canonicalAmount(got, signed)
		if err != nil || again != got {
			t.Fatal("normalization is not idempotent")
		}
		positive, err := positiveDecimal(input)
		wantPositive := input[0] != '-' && wantRat.Sign() > 0
		if (err == nil) != wantPositive || wantPositive && positive != got {
			t.Fatal("positive quantity validation disagrees with exact value")
		}
	})
}
