package fillledger

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	bolt "go.etcd.io/bbolt"
)

type faultDatabase struct {
	database
	mode string
	err  error
}

func (f *faultDatabase) Update(fn func(*bolt.Tx) error) error {
	switch f.mode {
	case "before-write":
		return f.err
	case "rollback-after-writes":
		return f.database.Update(func(tx *bolt.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			return f.err
		})
	case "lost-commit-result":
		if err := f.database.Update(fn); err != nil {
			return err
		}
		return f.err
	default:
		return f.database.Update(fn)
	}
}

func (f *faultDatabase) View(fn func(*bolt.Tx) error) error {
	if f.mode == "read-failure" {
		return f.err
	}
	return f.database.View(fn)
}

// These inject errors at the transaction API boundary, including both possible
// outcomes of failed writes/fsyncs. They are not kernel power-loss emulation.
func TestCommitIOFaultLatchesAndReopenResolvesOutcome(t *testing.T) {
	for _, fault := range []struct {
		name string
		err  error
	}{
		{"disk-full", syscall.ENOSPC}, {"short-write", io.ErrShortWrite},
		{"permission", os.ErrPermission}, {"sync-io", syscall.EIO},
	} {
		for _, mode := range []string{"before-write", "rollback-after-writes", "lost-commit-result"} {
			t.Run(fault.name+"/"+mode, func(t *testing.T) {
				s, path := newFixture(t)
				s.db = &faultDatabase{database: s.db, mode: mode, err: fault.err}
				snapshot, duplicate, err := s.Commit(0, fixtureFill(1), nextModel(t, 1))
				if !errors.Is(err, ErrUncertain) || !errors.Is(err, fault.err) || duplicate || snapshot.State != nil {
					t.Fatalf("uncertain result leaked state: snapshot=%+v duplicate=%v err=%v", snapshot, duplicate, err)
				}
				// Even a successful later read may not reopen the current instance.
				if !errors.Is(s.Health(), ErrUnavailable) {
					t.Fatal("storage fault remained healthy")
				}
				if _, err := s.Snapshot(); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("snapshot after fault err=%v", err)
				}
				if _, found, err := s.Lookup(1); found || !errors.Is(err, ErrUnavailable) {
					t.Fatalf("lookup after fault found=%v err=%v", found, err)
				}
				if _, _, err := s.Commit(0, fixtureFill(2), nextModel(t, 1)); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("commit after fault err=%v", err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				recovered, err := Open(path, fixtureScope())
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				before, err := recovered.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				wantBefore := 0
				if mode == "lost-commit-result" {
					wantBefore = 1
				}
				assertModel(t, before, wantBefore)
				_, found, err := recovered.Lookup(1)
				if err != nil || found != (wantBefore == 1) {
					t.Fatalf("torn receipt/checkpoint: found=%v err=%v", found, err)
				}
				after, duplicate, err := recovered.Commit(before.Revision, fixtureFill(1), nextModel(t, 1))
				if err != nil || duplicate != found {
					t.Fatalf("recovery replay duplicate=%v err=%v", duplicate, err)
				}
				assertModel(t, after, 1)
			})
		}
	}
}

func TestLookupFailureCannotBecomeUnseenOrder(t *testing.T) {
	s, _ := newFixture(t)
	s.db = &faultDatabase{database: s.db, mode: "read-failure", err: syscall.EIO}
	if _, found, err := s.Lookup(1); found || !errors.Is(err, syscall.EIO) {
		t.Fatalf("read failure found=%v err=%v", found, err)
	}
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed lookup allowed commit: %v", err)
	}
}

func TestClosedStoreCannotServeAsEmptyHistory(t *testing.T) {
	s, _ := newFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("lookup closed err=%v", err)
	}
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("commit closed err=%v", err)
	}
	if !errors.Is(s.Health(), ErrClosed) {
		t.Fatal("closed store healthy")
	}
}

func TestCorruptLedgerRefusesRecoveryWithoutRepair(t *testing.T) {
	for _, kind := range []string{"checkpoint-checksum", "receipt-checksum", "missing-receipt", "missing-bucket", "unknown-version", "receipt-key", "receipt-revision"} {
		t.Run(kind, func(t *testing.T) {
			s, path := newFixture(t)
			if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); err != nil {
				t.Fatal(err)
			}
			err := s.db.Update(func(tx *bolt.Tx) error {
				meta, completed := tx.Bucket(metaBucket), tx.Bucket(completedBucket)
				switch kind {
				case "checkpoint-checksum":
					value := bytes.Clone(meta.Get(checkpointKey))
					value = bytes.Replace(value, []byte(`"revision":1`), []byte(`"revision":2`), 1)
					return meta.Put(checkpointKey, value)
				case "receipt-checksum":
					value := bytes.Clone(completed.Get(orderKey(1)))
					value = bytes.Replace(value, []byte(`"executedQty":"0.1"`), []byte(`"executedQty":"0.2"`), 1)
					return completed.Put(orderKey(1), value)
				case "missing-receipt":
					return completed.Delete(orderKey(1))
				case "missing-bucket":
					return tx.DeleteBucket(completedBucket)
				case "unknown-version":
					return putSealed(meta, headerKey, header{Version: 2, Scope: fixtureScope()})
				case "receipt-key":
					if err := completed.Put(orderKey(2), bytes.Clone(completed.Get(orderKey(1)))); err != nil {
						return err
					}
					return completed.Delete(orderKey(1))
				case "receipt-revision":
					return putSealed(completed, orderKey(1), receipt{Completion: fixtureFill(1), Revision: 2})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			unexpected, err := Open(path, fixtureScope())
			if unexpected != nil {
				unexpected.Close()
				t.Fatal("corrupt ledger opened")
			}
			want := ErrCorrupt
			if kind == "unknown-version" {
				want = ErrVersion
			}
			if !errors.Is(err, want) {
				t.Fatalf("open err=%v want %v", err, want)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed recovery modified evidence")
			}
		})
	}
}

func TestTruncatedDataPagesRefuseOpenBeforeFreelistRead(t *testing.T) {
	s, path := newFixture(t)
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); err != nil {
		t.Fatal(err)
	}
	var truncatedSize int64
	if err := s.db.View(func(tx *bolt.Tx) error { truncatedSize = tx.Size() - 4096; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, truncatedSize); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := Open(path, fixtureScope()); !errors.Is(err, ErrCorrupt) {
		if unexpected != nil {
			unexpected.Close()
		}
		t.Fatalf("truncated open err=%v", err)
	}
}
