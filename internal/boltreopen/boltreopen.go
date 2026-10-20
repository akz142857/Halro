// Package boltreopen holds a bbolt file that can be closed and opened again
// while the process runs.
//
// bbolt's freelist records, in an in-memory map, the transaction that took
// each page it hands out again, and forgets the entry only when that page is
// freed a second time. A file that mostly grows — history.db, archive.db —
// rarely frees a page twice, so the map grows with every write since the file
// was opened and is only given back by closing it. Reopening on a timer bounds
// it by the writes of one period instead of the process's uptime.
package boltreopen

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// ErrClosed is returned by a DB that was closed.
var ErrClosed = errors.New("bolt file is closed")

// DB is a bbolt file every access to which goes through View or Update, so
// Reopen can tell when nothing is using it.
type DB struct {
	path    string
	options bbolt.Options
	// mu is held shared by every transaction and exclusively by Reopen and
	// Close. bbolt orders readers and the writer itself; this lock exists only
	// so the handle is never swapped under a transaction.
	mu sync.RWMutex
	db *bbolt.DB
	// identity is the file first opened. A reopen that finds another file at
	// the path — renamed into place while this one was open — refuses it.
	identity os.FileInfo
	// written and writeTime carry bbolt's page-write counters across
	// reopens, which restart them; they are exported as counters.
	written   int64
	writeTime time.Duration
	// err is why db is nil: closed, or a reopen that failed. Every access
	// returns it until a later Reopen succeeds.
	err error
}

// Open opens path with options.
func Open(path string, options bbolt.Options) (*DB, error) {
	db, err := bbolt.Open(path, 0o600, &options)
	if err != nil {
		return nil, err
	}
	identity, err := os.Stat(path)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DB{path: path, options: options, db: db, identity: identity}, nil
}

func (d *DB) View(fn func(*bbolt.Tx) error) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return d.err
	}
	return d.db.View(fn)
}

func (d *DB) Update(fn func(*bbolt.Tx) error) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.db == nil {
		return d.err
	}
	return d.db.Update(fn)
}

// Stats reports bbolt's counters. The page writes and their time count from
// the first open; every other counter restarts at each reopen.
func (d *DB) Stats() bbolt.Stats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var stats bbolt.Stats
	if d.db != nil {
		stats = d.db.Stats()
	}
	stats.TxStats.IncWrite(d.written)
	stats.TxStats.IncWriteTime(d.writeTime)
	return stats
}

// Reopen closes the file and opens it again, giving back what bbolt's
// freelist has accumulated. It never waits: while any transaction is running
// — an export can hold one for minutes — it does nothing and reports false,
// and the caller tries again on its next period.
//
// A reopen that fails leaves the DB closed, so every access errors instead of
// reading or writing something else; the next Reopen tries again.
func (d *DB) Reopen() (bool, error) {
	if !d.mu.TryLock() {
		return false, nil
	}
	defer d.mu.Unlock()
	if errors.Is(d.err, ErrClosed) {
		return false, ErrClosed
	}
	if d.db != nil {
		stats := d.db.Stats()
		d.written += stats.TxStats.GetWrite()
		d.writeTime += stats.TxStats.GetWriteTime()
		err := d.db.Close()
		d.db = nil
		if err != nil {
			d.err = fmt.Errorf("close %s for reopen: %w", d.path, err)
			return true, d.err
		}
	}
	db, err := bbolt.Open(d.path, 0o600, &d.options)
	if err != nil {
		d.err = fmt.Errorf("reopen %s: %w", d.path, err)
		return true, d.err
	}
	identity, err := os.Stat(d.path)
	if err != nil || !os.SameFile(identity, d.identity) {
		db.Close()
		if err == nil {
			err = errors.New("another file was put in its place while it was open")
		}
		d.err = fmt.Errorf("reopen %s: %w", d.path, err)
		return true, d.err
	}
	d.db, d.err = db, nil
	return true, nil
}

// Close waits for running transactions and closes the file.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = ErrClosed
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	return err
}
