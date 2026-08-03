package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the driver Open opens with

	"github.com/neodata-io/neokit/declare"
)

// ComponentName is what [Open] declares. Tests and scrape configs use it
// instead of retyping "database".
const ComponentName = "database"

// Open opens the service's database with the settings a server actually wants,
// runs migrate against it, and declares it as [ComponentName] — the boot report
// line, the readiness check and the shutdown step all come from this one call.
// A nil migrate skips the migration step.
//
//	db, err := sqlitex.Open(a, cfg.DatabasePath, migrate)
func Open(d declare.Declarer, path string, migrate func(*sql.DB) error) (*sql.DB, error) {
	return OpenNamed(d, ComponentName, path, migrate)
}

// OpenNamed is [Open] with an explicit component name, for the service that
// opens two databases. Nothing is declared if the open fails.
func OpenNamed(d declare.Declarer, name, path string, migrate func(*sql.DB) error) (*sql.DB, error) {
	// An empty path is never a legitimate target, and it is not harmless: SQLite
	// accepts it and hands back a private, anonymous database that accepts writes
	// and vanishes on restart. A deployment that forgot to set its path would
	// start, look healthy, serve traffic, and lose everything — silently. Ask for
	// a non-persistent database with ":memory:" instead, which says so.
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlitex: database path is empty (use \":memory:\" for a non-persistent database)")
	}

	memory := path == ":memory:"
	if !memory {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn(path, memory))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// sql.Open never dials, so nothing below has run yet. Force one connection
	// through it now, synchronously, before any caller can race it: switching a
	// brand-new file to WAL (in dsn, below) needs a brief exclusive lock, and
	// SQLite does not route that particular lock through the busy_timeout retry
	// loop — two connections racing to make the switch at once both return
	// SQLITE_BUSY immediately instead of one waiting for the other. Once this
	// first connection has made the switch, the file itself says WAL, so every
	// connection the pool opens later just confirms it — no lock, no race.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}

	if memory {
		// A bare :memory: database is private to each connection, so a pool would
		// hand out separate empty databases — a write on one invisible to the next
		// read. Pinning to one connection is what makes :memory: usable.
		db.SetMaxOpenConns(1)
	} else {
		// WAL allows one writer plus concurrent readers, so a large pool buys
		// nothing and costs file handles.
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}

	if migrate != nil {
		if err := migrate(db); err != nil {
			// Never hand back a handle onto a half-built schema.
			db.Close()
			return nil, fmt.Errorf("migrate schema: %w", err)
		}
	}

	// Last, so nothing is declared for a database that failed to open or migrate.
	declare.Add(d, name,
		declare.Detail(path),
		declare.Ready(db.PingContext),
		declare.Close(func(context.Context) error { return db.Close() }),
	)
	return db, nil
}

// dsn builds the connection string.
//
// The pragmas ride in the DSN rather than a post-open Exec, and that is the
// whole point of this function. Exec runs on exactly *one* pooled connection;
// every other connection the pool opens later would have no busy_timeout and no
// foreign-key enforcement, and nothing would say so — the second concurrent
// query simply behaves differently from the first.
func dsn(path string, memory bool) string {
	pragmas := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
		// An 8 MB page cache (negative = KiB, not pages) keeps hot scans resident
		// instead of re-reading pages; temp_store(memory) keeps the temp b-trees
		// an ORDER BY builds off the disk too. Both are per-connection, so like
		// the pragmas above they must ride the DSN.
		"_pragma=cache_size(-8000)",
		"_pragma=temp_store(memory)",
	}
	if memory {
		return "file::memory:?" + strings.Join(pragmas, "&")
	}
	pragmas = append(pragmas, "_pragma=journal_mode(WAL)")
	return "file:" + path + "?" + strings.Join(pragmas, "&")
}
