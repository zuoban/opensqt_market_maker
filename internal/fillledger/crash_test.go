package fillledger

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const crashPathEnv = "OPENSQT_TEST_LEDGER_CRASH_PATH"
const crashPhaseEnv = "OPENSQT_TEST_LEDGER_CRASH_PHASE"

type crashBeforeCommitDB struct{ database }

func (db crashBeforeCommitDB) Update(fn func(*bolt.Tx) error) error {
	return db.database.Update(func(tx *bolt.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		os.Exit(37) // Skip transaction commit, store.Close, and every defer.
		return nil
	})
}

func TestLedgerCrashChild(t *testing.T) {
	path := os.Getenv(crashPathEnv)
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(path, fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv(crashPhaseEnv)
	if phase == "before-commit" {
		os.Exit(37)
	}
	if phase == "in-transaction" {
		s.db = crashBeforeCommitDB{s.db}
	}
	if _, _, err := s.Commit(0, fixtureFill(1), nextModel(t, 1)); err != nil {
		t.Fatal(err)
	}
	if phase == "after-durable-before-publish" {
		os.Exit(37)
	}
	if phase == "after-publish" {
		state, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		assertModel(t, state, 1)
		os.Exit(37)
	}
	t.Fatalf("unknown crash phase %q", phase)
}

func TestAbruptProcessExitKeepsReceiptAndStateAtomic(t *testing.T) {
	for _, phase := range []string{"before-commit", "in-transaction", "after-durable-before-publish", "after-publish"} {
		t.Run(phase, func(t *testing.T) {
			s, path := newFixture(t)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerCrashChild$")
			cmd.Env = append(os.Environ(), crashPathEnv+"="+path, crashPhaseEnv+"="+phase)
			output, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
				t.Fatalf("child exit=%v output=%s", err, output)
			}
			recovered, err := Open(path, fixtureScope())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			state, err := recovered.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			if phase == "after-durable-before-publish" || phase == "after-publish" {
				count = 1
			}
			assertModel(t, state, count)
			_, found, err := recovered.Lookup(1)
			if err != nil || found != (count == 1) {
				t.Fatalf("torn recovery: count=%d found=%v err=%v", count, found, err)
			}
			state, duplicate, err := recovered.Commit(state.Revision, fixtureFill(1), nextModel(t, 1))
			if err != nil || duplicate != found {
				t.Fatalf("replay duplicate=%v err=%v", duplicate, err)
			}
			assertModel(t, state, 1)
		})
	}
}
