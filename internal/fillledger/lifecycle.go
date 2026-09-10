package fillledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
)

// Shared fail-closed file lifecycle for both incompatible prototype formats.
func createLedgerDB(path string, initialize func(*bolt.Tx) error) (*bolt.DB, error) {
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
	err = db.Update(initialize)
	if err == nil {
		// Persist the new directory entry as well as the database pages.
		err = syncDirectory(filepath.Dir(path))
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: initialize: %w", ErrUncertain, err)
	}
	return db, nil
}

func openLedgerDB(path string, validate func(*bolt.DB) error) (*bolt.DB, error) {
	// Probe before writable open loads free pages, then recheck under the writer lock.
	probe, err := openDB(path, false, true)
	if err != nil {
		return nil, err
	}
	err = errors.Join(validate(probe), probe.Close())
	if err != nil {
		return nil, err
	}
	db, err := openDB(path, false, false)
	if err != nil {
		return nil, err
	}
	if err := validate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func checkDatabasePages(tx *bolt.Tx, path string) error {
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
	return nil
}
