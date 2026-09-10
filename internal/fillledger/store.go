package fillledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	formatVersion = 1
	MaxStateBytes = 1 << 20 // Prototype checkpoints only; production needs slot-level writes.
)

var (
	ErrInvalid            = errors.New("invalid ledger input")
	ErrCorrupt            = errors.New("ledger integrity check failed")
	ErrScopeMismatch      = errors.New("ledger belongs to another scope")
	ErrVersion            = errors.New("unsupported ledger version")
	ErrRevisionConflict   = errors.New("stale checkpoint revision")
	ErrCompletionConflict = errors.New("completed order has conflicting execution data")
	ErrUncertain          = errors.New("ledger commit outcome is uncertain; reopen and reconcile")
	ErrUnavailable        = errors.New("ledger unavailable")
	ErrClosed             = errors.New("ledger closed")
	metaBucket            = []byte("meta")
	completedBucket       = []byte("completed")
	headerKey             = []byte("header")
	checkpointKey         = []byte("checkpoint")
)

type header struct {
	Version int   `json:"version"`
	Scope   Scope `json:"scope"`
}

// Snapshot is an independent copy. State must be a complete recoverable model,
// not just a balance or a list of dedup keys. Its semantics belong to the caller.
type Snapshot struct {
	Revision uint64 `json:"revision"`
	State    []byte `json:"state"`
}

type receipt struct {
	Completion Completion `json:"completion"`
	Revision   uint64     `json:"revision"`
}

type sealed struct {
	Payload json.RawMessage `json:"payload"`
	SHA256  [32]byte        `json:"sha256"`
}

// database permits fault injection in package tests at the transaction boundary.
// The production constructor always uses a syncing bbolt DB, never an async queue.
type database interface {
	View(func(*bolt.Tx) error) error
	Update(func(*bolt.Tx) error) error
	Close() error
}

type Store struct {
	mu     sync.Mutex
	db     database
	failed error // Sticky: only Close + Open may recover from storage uncertainty.
	closed bool
}

// Create is an explicit bootstrap operation. It refuses any existing file and
// does not create parent directories or silently replace incomplete databases.
func Create(path string, scope Scope, initialState []byte) (*Store, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	if err := validateState(initialState); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := openDB(path, true, false)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucket(completedBucket); err != nil {
			return err
		}
		if err := putSealed(meta, headerKey, header{Version: formatVersion, Scope: scope}); err != nil {
			return err
		}
		return putSealed(meta, checkpointKey, Snapshot{State: initialState})
	})
	if err == nil {
		// Sync the containing directory too: a synced file alone does not make
		// its newly created directory entry durable on Unix filesystems.
		err = syncDirectory(filepath.Dir(path))
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: initialize: %w", ErrUncertain, err)
	}
	return &Store{db: db}, nil
}

// Open only opens an existing ledger and validates it before exposing state.
// It never creates, truncates, repairs, or falls back to a fresh in-memory set.
func Open(path string, scope Scope) (*Store, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}
	// Validate in read-only mode first. A writable bbolt open loads free pages
	// immediately; truncated files must be rejected before that happens.
	probe, err := openDB(path, false, true)
	if err != nil {
		return nil, err
	}
	err = validateDB(probe, path, scope)
	err = errors.Join(err, probe.Close())
	if err != nil {
		return nil, err
	}
	db, err := openDB(path, false, false)
	if err != nil {
		return nil, err
	}
	// Recheck under the exclusive writer lock, including any legitimate
	// transaction committed between the read-only probe and writer acquisition.
	if err := validateDB(db, path, scope); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func validateDB(db *bolt.DB, path string, scope Scope) error {
	return db.View(func(tx *bolt.Tx) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if tx.Size() > info.Size() {
			return fmt.Errorf("%w: truncated database", ErrCorrupt)
		}
		// Drain the channel even after a failure so the checker goroutine exits.
		var checkErr error
		for err := range tx.Check() {
			if checkErr == nil {
				checkErr = err
			}
		}
		if checkErr != nil {
			return fmt.Errorf("%w: database pages: %w", ErrCorrupt, checkErr)
		}
		meta, completed := tx.Bucket(metaBucket), tx.Bucket(completedBucket)
		if meta == nil || completed == nil {
			return fmt.Errorf("%w: missing buckets", ErrCorrupt)
		}
		var h header
		if err := readSealed(meta.Get(headerKey), &h); err != nil {
			return err
		}
		if h.Version != formatVersion {
			return ErrVersion
		}
		if h.Scope != scope {
			return ErrScopeMismatch
		}
		snapshot, err := readSnapshot(tx)
		if err != nil {
			return err
		}
		var count uint64
		err = completed.ForEach(func(key, value []byte) error {
			_, err := readReceipt(key, value, snapshot.Revision)
			count++
			return err
		})
		if err != nil {
			return err
		}
		// This prototype commits exactly one completion per revision. A missing
		// receipt must not quietly become a future cache miss.
		if count != snapshot.Revision {
			return fmt.Errorf("%w: receipt/checkpoint count mismatch", ErrCorrupt)
		}
		return nil
	})
}

func openDB(path string, initializing, readOnly bool) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{
		Timeout:  250 * time.Millisecond,
		ReadOnly: readOnly,
		OpenFile: func(name string, flag int, mode os.FileMode) (*os.File, error) {
			// bbolt normally creates and initializes a missing/empty database.
			// Recovery forbids both, including a file disappearing during Open.
			f, err := os.OpenFile(name, flag&^os.O_CREATE, mode)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err == nil && (!info.Mode().IsRegular() || !initializing && info.Size() < 16384) {
				err = fmt.Errorf("%w: non-regular or incomplete file", ErrCorrupt)
			}
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			return f, nil
		},
	})
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Platforms/filesystems without directory fsync fail explicitly. This
	// prototype assumes Unix directory-sync semantics; Windows is unvalidated.
	return dir.Sync()
}

func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthLocked(); err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		snapshot, err = readSnapshot(tx)
		return err
	})
	if err != nil {
		return Snapshot{}, s.failLocked(err)
	}
	return snapshot, nil
}

// Lookup performs an exact indexed lookup, with no unbounded in-memory key cache.
func (s *Store) Lookup(orderID int64) (Completion, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthLocked(); err != nil {
		return Completion{}, false, err
	}
	if orderID <= 0 {
		return Completion{}, false, ErrInvalid
	}
	var found bool
	var r receipt
	err := s.db.View(func(tx *bolt.Tx) error {
		snapshot, err := readSnapshot(tx)
		if err != nil {
			return err
		}
		bucket := tx.Bucket(completedBucket)
		if bucket == nil {
			return ErrCorrupt
		}
		key := orderKey(orderID)
		value := bucket.Get(key)
		if value == nil {
			return nil
		}
		r, err = readReceipt(key, value, snapshot.Revision)
		found = err == nil
		return err
	})
	if err != nil {
		return Completion{}, false, s.failLocked(err)
	}
	return r.Completion, found, nil
}

// Commit atomically persists a completion and the model state that includes it.
// On duplicates it returns the CURRENT checkpoint and leaves nextState unused.
// Callers publish only the returned snapshot after err == nil; no side effects
// may precede commit. An I/O error could mean either committed or rolled back.
func (s *Store) Commit(expectedRevision uint64, fill Completion, nextState []byte) (snapshot Snapshot, duplicate bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthLocked(); err != nil {
		return Snapshot{}, false, err
	}
	fill, err = fill.canonical()
	if err != nil {
		return Snapshot{}, false, err
	}
	if err := validateState(nextState); err != nil {
		return Snapshot{}, false, err
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		current, err := readSnapshot(tx)
		if err != nil {
			return err
		}
		completed := tx.Bucket(completedBucket)
		if completed == nil {
			return ErrCorrupt
		}
		key := orderKey(fill.OrderID)
		if value := completed.Get(key); value != nil {
			r, err := readReceipt(key, value, current.Revision)
			if err != nil {
				return err
			}
			if r.Completion != fill {
				return ErrCompletionConflict
			}
			snapshot, duplicate = current, true
			return nil
		}
		if current.Revision != expectedRevision || current.Revision == math.MaxUint64 {
			return ErrRevisionConflict
		}
		snapshot = Snapshot{Revision: current.Revision + 1, State: bytes.Clone(nextState)}
		if err := putSealed(completed, key, receipt{Completion: fill, Revision: snapshot.Revision}); err != nil {
			return err
		}
		return putSealed(tx.Bucket(metaBucket), checkpointKey, snapshot)
	})
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			return Snapshot{}, false, err
		}
		return Snapshot{}, false, s.failLocked(errors.Join(ErrUncertain, err))
	}
	return snapshot, duplicate, nil
}

// Health only reports storage availability, not trading readiness or reconciliation.
func (s *Store) Health() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthLocked()
}

func (s *Store) healthLocked() error {
	if s.closed {
		return ErrClosed
	}
	return s.failed
}

func (s *Store) failLocked(err error) error {
	s.failed = errors.Join(ErrUnavailable, err)
	return s.failed
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

func validateState(state []byte) error {
	if len(state) == 0 || len(state) > MaxStateBytes {
		return fmt.Errorf("%w: checkpoint size", ErrInvalid)
	}
	return nil
}

func orderKey(id int64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], uint64(id))
	return key[:]
}

func putSealed(bucket *bolt.Bucket, key []byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(sealed{Payload: payload, SHA256: sha256.Sum256(payload)})
	if err != nil {
		return err
	}
	return bucket.Put(key, encoded)
}

func readSealed(value []byte, result any) error {
	if len(value) == 0 || len(value) > 2*MaxStateBytes {
		return fmt.Errorf("%w: record size", ErrCorrupt)
	}
	var envelope sealed
	if err := json.Unmarshal(value, &envelope); err != nil {
		return fmt.Errorf("%w: record encoding", ErrCorrupt)
	}
	if sha256.Sum256(envelope.Payload) != envelope.SHA256 {
		return fmt.Errorf("%w: record checksum", ErrCorrupt)
	}
	if err := json.Unmarshal(envelope.Payload, result); err != nil {
		return fmt.Errorf("%w: record payload", ErrCorrupt)
	}
	return nil
}

func readSnapshot(tx *bolt.Tx) (Snapshot, error) {
	var snapshot Snapshot
	bucket := tx.Bucket(metaBucket)
	if bucket == nil {
		return snapshot, ErrCorrupt
	}
	if err := readSealed(bucket.Get(checkpointKey), &snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := validateState(snapshot.State); err != nil {
		return Snapshot{}, errors.Join(ErrCorrupt, err)
	}
	return snapshot, nil
}

func readReceipt(key, value []byte, revision uint64) (receipt, error) {
	var r receipt
	if err := readSealed(value, &r); err != nil {
		return r, err
	}
	canonical, err := r.Completion.canonical()
	if err != nil || canonical != r.Completion || !bytes.Equal(key, orderKey(r.Completion.OrderID)) || r.Revision == 0 || r.Revision > revision {
		return receipt{}, fmt.Errorf("%w: receipt identity/revision", ErrCorrupt)
	}
	return r, nil
}
