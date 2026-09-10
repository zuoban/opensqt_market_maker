package fillledger

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"

	bolt "go.etcd.io/bbolt"
)

var (
	slotBucket       = []byte("slots")
	progressBucket   = []byte("orders")
	slotHeadKey      = []byte("slot-head")
	ErrOrderConflict = errors.New("order identity or progress conflicts with durable state")
)

// SlotStore is the incompatible v2 transaction prototype. It is intentionally
// not configured or opened by the trading process. Business DTOs are exercised
// by position's offline replay tests; startup/reservation recovery is separate.
type SlotStore struct{ store *Store }

// CreateSlots explicitly bootstraps slots with zero session accounting and no
// processed orders. It is NOT a migration/import of an existing trading session.
func CreateSlots(path string, scope Scope, initial []SlotWrite) (*SlotStore, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	slots, err := canonicalSlots(initial)
	if err != nil || len(slots) == 0 {
		return nil, ErrInvalid
	}
	db, err := createLedgerDB(path, func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		bucket, err := tx.CreateBucket(slotBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucket(progressBucket); err != nil {
			return err
		}
		if err := putSealed(meta, headerKey, header{Version: slotFormatVersion, Scope: scope}); err != nil {
			return err
		}
		for _, slot := range slots {
			if err := putSealed(bucket, []byte(slot.Key), SlotRecord{SlotWrite: slot}); err != nil {
				return err
			}
		}
		return putSealed(meta, slotHeadKey, slotHead{SlotCount: uint64(len(slots)), Totals: ZeroAccounting()})
	})
	if err != nil {
		return nil, err
	}
	return &SlotStore{store: &Store{db: db}}, nil
}

func OpenSlots(path string, scope Scope) (*SlotStore, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	db, err := openLedgerDB(path, func(db *bolt.DB) error { return validateSlotDB(db, path, scope) })
	if err != nil {
		return nil, err
	}
	return &SlotStore{store: &Store{db: db}}, nil
}

func validateSlotDB(db *bolt.DB, path string, scope Scope) error {
	return db.View(func(tx *bolt.Tx) error {
		if err := checkDatabasePages(tx, path); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return ErrCorrupt
		}
		var h header
		if err := readSealed(meta.Get(headerKey), &h); err != nil {
			return err
		}
		if h.Version != slotFormatVersion {
			return ErrVersion
		}
		if h.Scope != scope {
			return ErrScopeMismatch
		}
		head, err := readSlotHead(tx)
		if err != nil {
			return err
		}
		var slots, orders, filled, maxRevision uint64
		if err := tx.Bucket(slotBucket).ForEach(func(k, v []byte) error {
			_, err := readSlotRecord(k, v, head.Revision)
			slots++
			return err
		}); err != nil {
			return err
		}
		if err := tx.Bucket(progressBucket).ForEach(func(k, v []byte) error {
			o, err := readOrderRecord(k, v, head.Revision)
			if err != nil {
				return err
			}
			if tx.Bucket(slotBucket).Get([]byte(o.SlotKey)) == nil {
				return ErrCorrupt
			}
			orders++
			if o.Status == "FILLED" {
				filled++
			}
			maxRevision = max(maxRevision, o.Revision)
			return nil
		}); err != nil {
			return err
		}
		if slots != head.SlotCount || orders != head.OrderCount || filled != head.Totals.FilledOrders || maxRevision != head.Revision {
			return fmt.Errorf("%w: slot/order/head counts", ErrCorrupt)
		}
		return nil
	})
}

func readSlotHead(tx *bolt.Tx) (slotHead, error) {
	var h slotHead
	meta := tx.Bucket(metaBucket)
	if meta == nil || tx.Bucket(slotBucket) == nil || tx.Bucket(progressBucket) == nil {
		return h, ErrCorrupt
	}
	if err := readSealed(meta.Get(slotHeadKey), &h); err != nil {
		return slotHead{}, err
	}
	totals, err := canonicalAccounting(h.Totals)
	if err != nil || totals != h.Totals || h.SlotCount == 0 || h.OrderCount > h.Revision || h.Totals.FilledOrders > h.OrderCount {
		return slotHead{}, ErrCorrupt
	}
	return h, nil
}

func readSlotRecord(key, value []byte, revision uint64) (SlotRecord, error) {
	var r SlotRecord
	if err := readSealed(value, &r); err != nil {
		return SlotRecord{}, err
	}
	canonical, err := positiveDecimal(r.Key)
	if err != nil || canonical != r.Key || !bytes.Equal(key, []byte(r.Key)) || len(r.State) == 0 || len(r.State) > MaxSlotStateBytes || r.Revision > revision {
		return SlotRecord{}, ErrCorrupt
	}
	return r, nil
}

func readOrderRecord(key, value []byte, revision uint64) (OrderRecord, error) {
	var r OrderRecord
	if err := readSealed(value, &r); err != nil {
		return OrderRecord{}, err
	}
	canonical, err := canonicalOrder(r.OrderCheckpoint)
	if err != nil || !reflect.DeepEqual(canonical, r.OrderCheckpoint) || !bytes.Equal(key, orderKey(r.OrderID)) || r.Revision == 0 || r.Revision > revision || r.TransitionHash == ([32]byte{}) {
		return OrderRecord{}, ErrCorrupt
	}
	return r, nil
}

func readSlotView(tx *bolt.Tx, head slotHead, keys []string, ids []int64) (SlotView, error) {
	view := SlotView{Revision: head.Revision, Totals: head.Totals, Slots: make(map[string]SlotRecord, len(keys)), Orders: make(map[int64]OrderRecord, len(ids))}
	for _, key := range keys {
		if v := tx.Bucket(slotBucket).Get([]byte(key)); v != nil {
			r, err := readSlotRecord([]byte(key), v, head.Revision)
			if err != nil {
				return SlotView{}, err
			}
			view.Slots[key] = r
		}
	}
	for _, id := range ids {
		if v := tx.Bucket(progressBucket).Get(orderKey(id)); v != nil {
			r, err := readOrderRecord(orderKey(id), v, head.Revision)
			if err != nil {
				return SlotView{}, err
			}
			if tx.Bucket(slotBucket).Get([]byte(r.SlotKey)) == nil {
				return SlotView{}, ErrCorrupt
			}
			view.Orders[id] = r
		}
	}
	return view, nil
}

// Read is a consistent bounded read of selected slots/order progress and totals.
// Missing orders are distinct from storage errors, which latch unavailability.
func (s *SlotStore) Read(keys []string, ids []int64) (SlotView, error) {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.healthLocked(); err != nil {
		return SlotView{}, err
	}
	if len(keys) > MaxTransactionSlots || len(ids) > MaxTransactionSlots {
		return SlotView{}, ErrInvalid
	}
	canonicalKeys := make([]string, len(keys))
	for i, k := range keys {
		var err error
		canonicalKeys[i], err = positiveDecimal(k)
		if err != nil {
			return SlotView{}, err
		}
	}
	for _, id := range ids {
		if id <= 0 {
			return SlotView{}, ErrInvalid
		}
	}
	var view SlotView
	err := st.db.View(func(tx *bolt.Tx) error {
		head, err := readSlotHead(tx)
		if err != nil {
			return err
		}
		view, err = readSlotView(tx, head, canonicalKeys, ids)
		return err
	})
	if err != nil {
		return SlotView{}, st.failLocked(err)
	}
	return view, nil
}

// Commit writes only affected slots, one order's progress, and totals. Global
// revision CAS prevents stale cross-slot calculations from replacing new state.
// Exact last-transaction replay returns CURRENT rows, even at a stale revision.
// Older events must be read/reduced again by the caller after a revision conflict.
// Errors never return publishable state; I/O uncertainty latches the instance.
func (s *SlotStore) Commit(input SlotTransaction) (SlotCommitResult, error) {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.healthLocked(); err != nil {
		return SlotCommitResult{}, err
	}
	in, hash, err := canonicalTransaction(input)
	if err != nil {
		return SlotCommitResult{}, err
	}
	keys := make([]string, len(in.Slots))
	for i, slot := range in.Slots {
		keys[i] = slot.Key
	}
	var result SlotCommitResult
	err = st.db.Update(func(tx *bolt.Tx) error {
		head, err := readSlotHead(tx)
		if err != nil {
			return err
		}
		orders := tx.Bucket(progressBucket)
		value := orders.Get(orderKey(in.Order.OrderID))
		var previous OrderRecord
		if value != nil {
			previous, err = readOrderRecord(orderKey(in.Order.OrderID), value, head.Revision)
			if err != nil {
				return err
			}
			if previous.ClientOrderID != in.Order.ClientOrderID || previous.Side != in.Order.Side || previous.SlotKey != in.Order.SlotKey {
				return ErrOrderConflict
			}
			if previous.TransitionHash == hash {
				result.View, err = readSlotView(tx, head, keys, []int64{in.Order.OrderID})
				result.Duplicate = true
				return err
			}
		}
		if in.ExpectedRevision != head.Revision || head.Revision == math.MaxUint64 {
			return ErrRevisionConflict
		}
		if value != nil {
			qtyCmp := amountRat(in.Order.ExecutedQty).Cmp(amountRat(previous.ExecutedQty))
			quoteCmp := amountRat(in.Order.ExecutedQuote).Cmp(amountRat(previous.ExecutedQuote))
			if previous.Status == "FILLED" || qtyCmp < 0 || quoteCmp < 0 || qtyCmp == 0 && quoteCmp != 0 || qtyCmp > 0 && quoteCmp <= 0 {
				return ErrOrderConflict
			}
			if terminalStatus(previous.Status) && !terminalStatus(in.Order.Status) {
				return ErrOrderConflict
			}
			// At unchanged terminal quantity, only a provably newer event may
			// correct accounting. A fresh global revision alone is not evidence.
			if terminalStatus(previous.Status) && qtyCmp == 0 &&
				(previous.UpdateTime == 0 || in.Order.UpdateTime <= previous.UpdateTime) {
				return ErrOrderConflict
			}
			if in.Order.UpdateTime < previous.UpdateTime {
				return ErrOrderConflict
			}
			if reflect.DeepEqual(previous.OrderCheckpoint, in.Order) {
				return ErrOrderConflict
			}
		}
		expectedFills := uint64(0)
		if in.Order.Status == "FILLED" {
			expectedFills = 1
		}
		if in.Delta.FilledOrders != expectedFills {
			return ErrOrderConflict
		}
		// Enforce the quantity delta against authoritative cumulative progress too.
		oldQty := "0"
		if value != nil {
			oldQty = previous.ExecutedQty
		}
		qtyDelta := new(big.Rat).Sub(amountRat(in.Order.ExecutedQty), amountRat(oldQty))
		appliedQty, otherQty := in.Delta.BuyQty, in.Delta.SellQty
		if in.Order.Side == "SELL" {
			appliedQty, otherQty = in.Delta.SellQty, in.Delta.BuyQty
		}
		if amountRat(appliedQty).Cmp(qtyDelta) != 0 || otherQty != "0" {
			return ErrOrderConflict
		}
		if in.Order.Side == "BUY" && in.Delta.RealizedPNL != "0" {
			return ErrOrderConflict
		}
		if head.Totals.BuyQty, err = addAmount(head.Totals.BuyQty, in.Delta.BuyQty); err != nil {
			return err
		}
		if head.Totals.SellQty, err = addAmount(head.Totals.SellQty, in.Delta.SellQty); err != nil {
			return err
		}
		if head.Totals.RealizedPNL, err = addAmount(head.Totals.RealizedPNL, in.Delta.RealizedPNL); err != nil {
			return err
		}
		if head.Totals.FilledOrders > math.MaxUint64-in.Delta.FilledOrders {
			return ErrOrderConflict
		}
		head.Totals.FilledOrders += in.Delta.FilledOrders
		head.Revision++
		if value == nil {
			head.OrderCount++
		}
		bucket := tx.Bucket(slotBucket)
		for _, slot := range in.Slots {
			if value := bucket.Get([]byte(slot.Key)); value == nil {
				head.SlotCount++
			} else if _, err := readSlotRecord([]byte(slot.Key), value, head.Revision-1); err != nil {
				return err
			}
			if err := putSealed(bucket, []byte(slot.Key), SlotRecord{SlotWrite: slot, Revision: head.Revision}); err != nil {
				return err
			}
		}
		record := OrderRecord{OrderCheckpoint: in.Order, Revision: head.Revision, TransitionHash: hash}
		if err := putSealed(orders, orderKey(in.Order.OrderID), record); err != nil {
			return err
		}
		if err := putSealed(tx.Bucket(metaBucket), slotHeadKey, head); err != nil {
			return err
		}
		result.View, err = readSlotView(tx, head, keys, []int64{in.Order.OrderID})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			return SlotCommitResult{}, err
		}
		return SlotCommitResult{}, st.failLocked(errors.Join(ErrUncertain, err))
	}
	return result, nil
}

func (s *SlotStore) Health() error { return s.store.Health() }
func (s *SlotStore) Close() error  { return s.store.Close() }
