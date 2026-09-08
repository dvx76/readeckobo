// Package store is the readeckobo server-side state store, backed by SQLite
// through the pure-Go modernc.org/sqlite driver (no cgo, on-device safe).
//
// It keeps three ledgers/caches:
//
//   - articles_seen   — the per-user/device "seen" ledger used by the agent
//     state feed to decide add/update/remove actions;
//   - kepub_cache     — generated kepub artifacts (epub + span map) keyed by
//     bookmark_id + etag, so the same article is not re-generated on every
//     kepub download or annotation sync;
//   - annotation_ledger — maps a device Bookmark row (device + bookmark_row_id
//     UUID) to the Readeck annotation id we created for it, so re-posts can be
//     detected as unchanged/updated instead of duplicated.
//
// The store file lives under the configured server.data_dir (default "data",
// see internal/config). The database runs in WAL mode; every connection gets a
// busy timeout and NORMAL synchronous mode via the DSN _pragma parameters.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// dbFileName is the SQLite database file written inside data_dir.
const dbFileName = "readeckobo.sqlite"

// Store wraps the SQLite database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the store database inside dataDir.
//
// dataDir may be ":memory:" (a shared-cache in-memory database) for tests.
// The database file is dataDir/readeckobo.sqlite; dataDir is created if it
// does not exist.
func Open(dataDir string) (*Store, error) {
	var dsn string
	if dataDir == "" || dataDir == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	} else {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create data dir %s: %w", dataDir, err)
		}
		dsn = "file:" + filepath.Join(dataDir, dbFileName) +
			"?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=journal_mode(WAL)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// createSchema applies the store schema idempotently.
func (s *Store) createSchema() error {
	const schema = `
CREATE TABLE IF NOT EXISTS articles_seen (
    user_token_hash TEXT NOT NULL,
    device          TEXT NOT NULL,
    bookmark_id     TEXT NOT NULL,
    updated         TEXT NOT NULL,
    action          TEXT NOT NULL,
    PRIMARY KEY (user_token_hash, device, bookmark_id)
);

CREATE TABLE IF NOT EXISTS kepub_cache (
    bookmark_id TEXT PRIMARY KEY,
    etag        TEXT NOT NULL,
    epub        BLOB NOT NULL,
    span_map    TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS annotation_ledger (
    device          TEXT NOT NULL,
    bookmark_row_id TEXT NOT NULL,
    bookmark_id     TEXT NOT NULL,
    annotation_id   TEXT NOT NULL,
    date_modified   TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (device, bookmark_row_id)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: create schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// articles_seen (agent state feed ledger)
// ---------------------------------------------------------------------------

// SeenRow is one entry of the per-user/device seen ledger.
type SeenRow struct {
	BookmarkID string
	Updated    string
	Action     string
}

// GetSeen returns all seen-ledger rows for a user/device pair.
func (s *Store) GetSeen(ctx context.Context, userTokenHash, device string) ([]SeenRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT bookmark_id, updated, action FROM articles_seen
		 WHERE user_token_hash = ? AND device = ?`,
		userTokenHash, device)
	if err != nil {
		return nil, fmt.Errorf("store: query seen ledger: %w", err)
	}
	defer rows.Close()

	var out []SeenRow
	for rows.Next() {
		var r SeenRow
		if err := rows.Scan(&r.BookmarkID, &r.Updated, &r.Action); err != nil {
			return nil, fmt.Errorf("store: scan seen row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate seen ledger: %w", err)
	}
	return out, nil
}

// UpsertSeen records (or refreshes) one seen-ledger entry.
func (s *Store) UpsertSeen(ctx context.Context, userTokenHash, device, bookmarkID, updated, action string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO articles_seen (user_token_hash, device, bookmark_id, updated, action)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_token_hash, device, bookmark_id)
		 DO UPDATE SET updated = excluded.updated, action = excluded.action`,
		userTokenHash, device, bookmarkID, updated, action)
	if err != nil {
		return fmt.Errorf("store: upsert seen ledger: %w", err)
	}
	return nil
}

// DeleteSeen removes one seen-ledger entry (used after an item is removed).
func (s *Store) DeleteSeen(ctx context.Context, userTokenHash, device, bookmarkID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM articles_seen WHERE user_token_hash = ? AND device = ? AND bookmark_id = ?`,
		userTokenHash, device, bookmarkID)
	if err != nil {
		return fmt.Errorf("store: delete seen row: %w", err)
	}
	return nil
}

// HashToken returns a stable identifier for a token that can safely be stored
// in the ledger without persisting the secret itself.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// kepub_cache
// ---------------------------------------------------------------------------

// KepubEntry is a cached kepub artifact for one bookmark (and etag).
type KepubEntry struct {
	BookmarkID string
	Etag       string
	EPUB       []byte
	SpanMap    string
	CreatedAt  time.Time
}

// GetKepub returns the cached artifact for a bookmark, or nil when absent.
func (s *Store) GetKepub(ctx context.Context, bookmarkID string) (*KepubEntry, error) {
	var e KepubEntry
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT bookmark_id, etag, epub, span_map, created_at FROM kepub_cache WHERE bookmark_id = ?`,
		bookmarkID).Scan(&e.BookmarkID, &e.Etag, &e.EPUB, &e.SpanMap, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query kepub cache: %w", err)
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &e, nil
}

// PutKepub upserts a kepub artifact for a bookmark.
func (s *Store) PutKepub(ctx context.Context, bookmarkID, etag string, epub []byte, spanMap string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kepub_cache (bookmark_id, etag, epub, span_map, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(bookmark_id)
		 DO UPDATE SET etag = excluded.etag, epub = excluded.epub,
		               span_map = excluded.span_map, created_at = excluded.created_at`,
		bookmarkID, etag, epub, spanMap, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: upsert kepub cache: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// annotation_ledger
// ---------------------------------------------------------------------------

// AnnotationLedgerRow maps a device Bookmark row to the Readeck annotation we
// created for it.
type AnnotationLedgerRow struct {
	Device        string
	BookmarkRowID string
	BookmarkID    string
	AnnotationID  string
	DateModified  string
	UpdatedAt     time.Time
}

// GetAnnotationLedger returns the ledger row for a device bookmark row, or nil
// when the row has not been ingested yet.
func (s *Store) GetAnnotationLedger(ctx context.Context, device, bookmarkRowID string) (*AnnotationLedgerRow, error) {
	var r AnnotationLedgerRow
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT device, bookmark_row_id, bookmark_id, annotation_id, date_modified, updated_at
		 FROM annotation_ledger WHERE device = ? AND bookmark_row_id = ?`,
		device, bookmarkRowID).Scan(&r.Device, &r.BookmarkRowID, &r.BookmarkID, &r.AnnotationID, &r.DateModified, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: query annotation ledger: %w", err)
	}
	r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &r, nil
}

// PutAnnotationLedger upserts the ledger entry for a device bookmark row.
func (s *Store) PutAnnotationLedger(ctx context.Context, device, bookmarkRowID, bookmarkID, annotationID, dateModified string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO annotation_ledger (device, bookmark_row_id, bookmark_id, annotation_id, date_modified, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device, bookmark_row_id)
		 DO UPDATE SET bookmark_id = excluded.bookmark_id,
		               annotation_id = excluded.annotation_id,
		               date_modified = excluded.date_modified,
		               updated_at = excluded.updated_at`,
		device, bookmarkRowID, bookmarkID, annotationID, dateModified, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: upsert annotation ledger: %w", err)
	}
	return nil
}
