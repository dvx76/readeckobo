package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver (CGO_ENABLED=0 builds)
)

// This file implements the WAL-safe read recipe from
// docs/research/kobo-db-schema.md §9.2: while Nickel runs, KoboReader.sqlite
// lives in WAL mode with uncheckpointed pages in the -wal/-shm siblings. We
// copy the database (plus its WAL/SHM companions) to a private snapshot and
// open the *copy* read-only. A torn copy (Nickel writing mid-copy) fails to
// open and is retried with a fresh copy; SQLite itself tolerates torn WAL
// tails via frame checksums.

// snapshotKoboDB copies src (and, when present, its -wal and -shm siblings)
// onto dst, then verifies the copy opens read-only and answers a trivial
// query. It retries from scratch a few times so a transient mid-copy write
// from Nickel cannot produce a poisoned snapshot.
func snapshotKoboDB(ctx context.Context, src, dst string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := copyKoboTree(src, dst); err != nil {
			lastErr = err
		} else {
			db, err := openReadOnly(dst)
			if err == nil {
				var n int
				err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n)
				_ = db.Close()
			}
			if err == nil {
				return nil
			}
			lastErr = fmt.Errorf("snapshot %s did not open cleanly: %v", dst, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Duration(attempt) * time.Millisecond):
		}
	}
	return fmt.Errorf("cannot create a readable snapshot of %s after 3 attempts: %v", src, lastErr)
}

// copyKoboTree copies src → dst plus, when they exist at copy time,
// src-wal → dst-wal and src-shm → dst-shm. The main file is copied first and
// the WAL last so the pair is as close to a single point in time as a plain
// file copy can be.
func copyKoboTree(src, dst string) error {
	removeSnapshot(dst)
	if err := copyFile(src, dst); err != nil {
		return err
	}
	if st, err := os.Stat(src + "-wal"); err == nil && !st.IsDir() {
		_ = copyFile(src+"-wal", dst+"-wal")
	}
	if st, err := os.Stat(src + "-shm"); err == nil && !st.IsDir() {
		_ = copyFile(src+"-shm", dst+"-shm") // best effort: SQLite can rebuild it
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeSnapshot drops a previous snapshot and its WAL/SHM siblings.
func removeSnapshot(dst string) {
	for _, p := range []string{dst, dst + "-wal", dst + "-shm"} {
		_ = os.Remove(p)
	}
}

func sqliteDSN(path string, ro bool) string {
	if ro {
		return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(4000)"
	}
	return "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(15000)"
}

// openReadOnly opens a sqlite database read-only (mode=ro in the URI).
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, true))
	if err != nil {
		return nil, err
	}
	// Fail fast on obviously missing/broken files instead of surfacing the
	// error at the first query.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openReadWrite opens the (live, device-side) database for the maintenance
// writes (DateCreated, Shelf/ShelfContent). WAL mode allows one writer at a
// time; busy_timeout makes us wait for Nickel instead of erroring.
func openReadWrite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// bookRowInfo is a content-table book row (VolumeIndex=-1) that lives under
// our managed directory.
type bookRowInfo struct {
	ContentID   string // stored form, e.g. file:///mnt/onboard/.kobo/readeck/x.kepub.epub
	DateCreated string
	SyncTime    string
	BookTitle   string
}

// managedBookRowQuery finds book rows under the agent's directory. The
// directory is unique to this agent, so contentID LIKE '%/.kobo/readeck/%'
// cannot match rows the agent did not import. It works regardless of the
// exact onboard mount path, which tests exploit by overriding --onboard.
const managedBookRowQuery = `
SELECT ContentID, COALESCE(DateCreated, ''), COALESCE(___SyncTime, ''), COALESCE(BookTitle, '')
FROM content
WHERE ContentID LIKE '%/.kobo/readeck/%'
  AND (VolumeIndex = -1 OR VolumeIndex IS NULL)
ORDER BY ContentID`

// readManagedBookRows returns all book rows under the managed directory.
func readManagedBookRows(ctx context.Context, db *sql.DB) (map[string]bookRowInfo, error) {
	rows, err := db.QueryContext(ctx, managedBookRowQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bookRowInfo{}
	for rows.Next() {
		var br bookRowInfo
		if err := rows.Scan(&br.ContentID, &br.DateCreated, &br.SyncTime, &br.BookTitle); err != nil {
			return nil, err
		}
		out[filenameOfVolumeID(br.ContentID)] = br
	}
	return out, rows.Err()
}

// dbVersion returns the device schema version (DbVersion table). Calibre
// switches the Shelf column set at version >= 64.
func dbVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT version FROM DbVersion LIMIT 1").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("reading DbVersion: %w", err)
	}
	if !v.Valid {
		return 0, fmt.Errorf("DbVersion table is empty")
	}
	return int(v.Int64), nil
}
