// Package store is duva's state: one SQLite database in /data.
//
// It replaces v4's JSON files -- the queue's queue.json and the watcher's
// detector.json -- with tables that can hold several releases per service, a
// blocklist and a history, and with transactions, so the watcher handing a
// finding to the queue and advancing its cutoff happen together or not at all.
// See docs/duva-v5-one-binary.md, phase 2.
//
// This package owns the database and its schema. What goes in which table is
// the business of the component that owns it, through methods here; no
// component writes another's tables.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	// Pure Go, so CGO_ENABLED=0 builds and cross-compiles keep working.
	// Chosen by measurement: see the design note.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the open database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it if needed, and brings its
// schema up to date.
//
// One connection. duva is one process with one writer, and SQLite serialises
// writers anyway; a pool would only add "database is locked" between this
// process's own goroutines. WAL so a long read never blocks a write, foreign
// keys on because SQLite has them off by default, and a busy timeout for the
// rare moment a backup holds a lock.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("bringing %s up to date: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Tx runs fn in a transaction, committing if it returns nil and rolling back
// otherwise.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Version is the schema version the database is at.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// migrate applies every migration newer than the database, in order, each in
// its own transaction with the version it brings the schema to.
//
// Numbered files, not a framework: 0001_initial.sql, 0002_... A migration is
// never edited once released, only followed by another.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	)`); err != nil {
		return err
	}
	current, err := s.Version()
	if err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, file := range files {
		version, err := versionOf(file)
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		body, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		err = s.Tx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, version)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	return nil
}

// versionOf reads the number a migration file starts with.
func versionOf(file string) (int, error) {
	base := file[strings.LastIndex(file, "/")+1:]
	digits, _, ok := strings.Cut(base, "_")
	if !ok {
		return 0, fmt.Errorf("%s: a migration is named NNNN_what.sql", file)
	}
	return strconv.Atoi(digits)
}
