package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureShelfModern exercises the calibre shelf-create pattern on the
// modern (DbVersion >= 64) schema and the idempotent re-add of a deleted
// shelf.
func TestEnsureShelfModern(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	db := openFixtureRW(t, dbPath)

	version, err := dbVersion(context.Background(), db)
	if err != nil || version < 64 {
		t.Fatalf("fixture DbVersion = %d (%v); expected modern >= 64", version, err)
	}

	now := time.Date(2026, 9, 8, 6, 11, 37, 0, time.UTC)
	// The pristine fixture pre-seeds a "Readeck" shelf whose Id is a
	// placeholder equal to the name (see docs/research/kobo-fixture-db.md);
	// drop it so this test exercises the agent's insert path and can assert
	// on the UUID the agent generates.
	execDB(t, db, `DELETE FROM ShelfContent`)
	execDB(t, db, `DELETE FROM Shelf`)
	if err := ensureShelf(context.Background(), db, "Readeck", now, version); err != nil {
		t.Fatalf("ensureShelf: %v", err)
	}
	// Modern shape stores Id and Type = UserTag.
	if got := scanOne(t, db, `SELECT count(*) FROM Shelf WHERE Name = 'Readeck' AND Type = 'UserTag' AND _IsDeleted = 'false'`); got != "1" {
		t.Fatalf("modern shelf row = %s", got)
	}
	// The inserted Id is a well-formed random v4 UUID, distinct from the name.
	if id := scanOne(t, db, `SELECT Id FROM Shelf WHERE Name = 'Readeck'`); !isUUIDv4(t, id) || id == "Readeck" {
		t.Fatalf("shelf Id = %q, want a well-formed UUID distinct from the shelf name", id)
	}
	// Second call must not duplicate.
	if err := ensureShelf(context.Background(), db, "Readeck", now, version); err != nil {
		t.Fatalf("ensureShelf (2nd): %v", err)
	}
	if got := scanOne(t, db, `SELECT count(*) FROM Shelf WHERE Name = 'Readeck'`); got != "1" {
		t.Fatalf("shelf duplicated: count=%s", got)
	}
	// A deleted shelf gets revived.
	execDB(t, db, `UPDATE Shelf SET _IsDeleted = 'true' WHERE Name = 'Readeck'`)
	if err := ensureShelf(context.Background(), db, "Readeck", now, version); err != nil {
		t.Fatalf("ensureShelf (revive): %v", err)
	}
	if got := scanOne(t, db, `SELECT _IsDeleted FROM Shelf WHERE Name = 'Readeck'`); got != "false" {
		t.Fatalf("shelf not revived: _IsDeleted=%s", got)
	}
}

// TestNewUUIDv4 asserts the RFC 4122 v4 shape of newUUIDv4 (length 36,
// hyphens at positions 8/13/18/23, hex elsewhere, version/variant bits set)
// and that two calls do not collide.
func TestNewUUIDv4(t *testing.T) {
	first, err := newUUIDv4()
	if err != nil {
		t.Fatalf("newUUIDv4: %v", err)
	}
	if !isUUIDv4(t, first) {
		t.Fatalf("first UUID %q is not well-formed", first)
	}
	// Version nibble at position 14 (index 12) must be '4', variant at
	// position 19 (index 14) must be 8/9/a/b.
	if first[14] != '4' {
		t.Errorf("UUID %q version nibble = %c, want 4", first, first[14])
	}
	switch first[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("UUID %q variant nibble = %c, want 8/9/a/b", first, first[19])
	}
	second, err := newUUIDv4()
	if err != nil {
		t.Fatalf("newUUIDv4 (2nd): %v", err)
	}
	if first == second {
		t.Fatalf("two newUUIDv4 calls returned the same value %q", first)
	}
}

// isUUIDv4 checks the string form of a version-4 UUID: length 36, hyphens at
// positions 8/13/18/23 (0-based) and hex digits everywhere else.
func isUUIDv4(t *testing.T, s string) bool {
	t.Helper()
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return false
			}
		}
	}
	return true
}

// TestEnsureShelfLegacy uses a database with the old shelf schema (no Id/Type
// columns) and DbVersion < 64, proving the agent falls back to the legacy
// INSERT shape instead of failing.
func TestEnsureShelfLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.sqlite")

	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execDB(t, db, `CREATE TABLE DbVersion (version INTEGER NOT NULL)`)
	execDB(t, db, `INSERT INTO DbVersion (version) VALUES (13)`)
	execDB(t, db, `CREATE TABLE Shelf (
		CreationDate TEXT NOT NULL, InternalName TEXT NOT NULL, LastModified TEXT NOT NULL,
		Name TEXT NOT NULL, _IsDeleted TEXT NOT NULL, _IsVisible TEXT NOT NULL, _IsSynced TEXT NOT NULL)`)
	execDB(t, db, `CREATE TABLE ShelfContent (
		ShelfName TEXT NOT NULL, ContentId TEXT NOT NULL, DateModified TEXT NOT NULL,
		_IsDeleted TEXT NOT NULL, _IsSynced TEXT NOT NULL)`)

	now := time.Date(2026, 9, 8, 6, 11, 37, 0, time.UTC)
	if err := ensureShelf(context.Background(), db, "Legacy Shelf", now, 13); err != nil {
		t.Fatalf("legacy ensureShelf: %v", err)
	}
	if got := scanOne(t, db, `SELECT count(*) FROM Shelf WHERE Name = 'Legacy Shelf' AND _IsDeleted = 'false'`); got != "1" {
		t.Fatalf("legacy shelf row = %s", got)
	}
	if err := addContentToShelf(context.Background(), db, "Legacy Shelf", "file:///x/book.kepub.epub", formatKoboTime(now)); err != nil {
		t.Fatalf("legacy addContentToShelf: %v", err)
	}
	if got := scanOne(t, db, `SELECT _IsDeleted FROM ShelfContent WHERE ShelfName='Legacy Shelf' AND ContentId='file:///x/book.kepub.epub'`); got != "false" {
		t.Fatalf("legacy membership = %s", got)
	}
}

// TestSetDateCreated asserts the one-shot DateCreated write and that it only
// ever targets book rows (VolumeIndex -1).
func TestSetDateCreated(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	db := openFixtureRW(t, dbPath)

	contentID := "file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub"
	if err := setDateCreated(context.Background(), db, contentID, "2026-09-07T10:00:00Z"); err != nil {
		t.Fatalf("setDateCreated: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`, contentID); got != "2026-09-07T10:00:00Z" {
		t.Fatalf("DateCreated = %s", got)
	}
	// A chapter row (VolumeIndex 0) must not be touched by the same id-less
	// update guard: updating a chapter ContentID returns ErrNoRows.
	if err := setDateCreated(context.Background(), db, contentID+"!!OEBPS/xhtml/ch001.xhtml", "2026-01-01T00:00:00Z"); err != sql.ErrNoRows {
		t.Fatalf("chapter update err = %v, want ErrNoRows", err)
	}
	if err := setDateCreated(context.Background(), db, "file:///does/not/exist.kepub.epub", "2026-01-01T00:00:00Z"); err != sql.ErrNoRows {
		t.Fatalf("missing row err = %v, want ErrNoRows", err)
	}
}

// TestSetBookAddedDate asserts the date-added ensure writes both sort-relevant
// columns (___SyncTime + DateCreated) and only targets book rows.
func TestSetBookAddedDate(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	db := openFixtureRW(t, dbPath)

	contentID := "file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub"
	if err := setBookAddedDate(context.Background(), db, contentID, "2026-09-01T08:00:00Z"); err != nil {
		t.Fatalf("setBookAddedDate: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`, contentID); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("DateCreated = %s", got)
	}
	if got := scanOne(t, db, `SELECT ___SyncTime FROM content WHERE ContentID = ? AND VolumeIndex = -1`, contentID); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("___SyncTime = %s", got)
	}
	if err := setBookAddedDate(context.Background(), db, contentID+"!!OEBPS/xhtml/ch001.xhtml", "2026-01-01T00:00:00Z"); err != sql.ErrNoRows {
		t.Fatalf("chapter update err = %v, want ErrNoRows", err)
	}
	if err := setBookAddedDate(context.Background(), db, "file:///does/not/exist.kepub.epub", "2026-01-01T00:00:00Z"); err != sql.ErrNoRows {
		t.Fatalf("missing row err = %v, want ErrNoRows", err)
	}
}

// TestAddContentToShelfDateModified asserts the shelf membership upsert writes
// the Readeck date-added (not the sync time) and converges existing rows.
func TestAddContentToShelfDateModified(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	db := openFixtureRW(t, dbPath)
	ctx := context.Background()

	shelf := "Readeck"
	contentID := "file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub"
	// Pristine fixture already has a membership; converge it to the article date.
	if err := addContentToShelf(ctx, db, shelf, contentID, "2026-09-01T08:00:00Z"); err != nil {
		t.Fatalf("addContentToShelf: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateModified FROM ShelfContent WHERE ShelfName = ? AND ContentId = ?`, shelf, contentID); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("DateModified = %s, want article created", got)
	}
	// Idempotent when already correct.
	if err := addContentToShelf(ctx, db, shelf, contentID, "2026-09-01T08:00:00Z"); err != nil {
		t.Fatalf("addContentToShelf idempotent: %v", err)
	}
	// New membership inserts with the article date.
	if err := addContentToShelf(ctx, db, shelf, "file:///x/new.kepub.epub", "2026-08-15T12:30:00Z"); err != nil {
		t.Fatalf("addContentToShelf insert: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateModified FROM ShelfContent WHERE ShelfName = ? AND ContentId = ?`, shelf, "file:///x/new.kepub.epub"); got != "2026-08-15T12:30:00Z" {
		t.Fatalf("inserted DateModified = %s", got)
	}
	// Deleted membership is revived with the article date.
	execDB(t, db, `UPDATE ShelfContent SET _IsDeleted = 'true' WHERE ShelfName = ? AND ContentId = ?`, shelf, contentID)
	if err := addContentToShelf(ctx, db, shelf, contentID, "2026-09-02T00:00:00Z"); err != nil {
		t.Fatalf("addContentToShelf revive: %v", err)
	}
	if got := scanOne(t, db, `SELECT _IsDeleted FROM ShelfContent WHERE ShelfName = ? AND ContentId = ?`, shelf, contentID); got != "false" {
		t.Fatalf("revived _IsDeleted = %s", got)
	}
	if got := scanOne(t, db, `SELECT DateModified FROM ShelfContent WHERE ShelfName = ? AND ContentId = ?`, shelf, contentID); got != "2026-09-02T00:00:00Z" {
		t.Fatalf("revived DateModified = %s", got)
	}
}

// TestSnapshotWalReplay proves the WAL-safe recipe: a database with a live,
// uncheckpointed WAL (writer connection still open) is copied and the copy
// sees the uncheckpointed rows.
func TestSnapshotWalReplay(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)

	writer, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	execDB(t, writer, "PRAGMA journal_mode = WAL")
	execDB(t, writer, "PRAGMA wal_autocheckpoint = 0")
	// Uncheckpointed insert: it lives only in the -wal file while open.
	execDB(t, writer, `INSERT INTO content
		(ContentID, ContentType, MimeType, BookID, BookTitle, Title, Attribution, DateCreated, VolumeIndex, ___UserID)
		VALUES ('file:///tmp/wal-test.kepub.epub','6','application/x-kobo-epub+zip','b','WAL Test','WAL Test','T','2026-09-09T00:00:00Z',-1,'')`)

	snap := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := snapshotKoboDB(context.Background(), dbPath, snap); err != nil {
		t.Fatalf("snapshotKoboDB: %v", err)
	}
	ro, err := openReadOnly(snap)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer ro.Close()
	if got := scanOne(t, ro, `SELECT count(*) FROM content WHERE ContentID = 'file:///tmp/wal-test.kepub.epub'`); got != "1" {
		t.Fatalf("snapshot missing uncheckpointed row (count=%s)", got)
	}
}

// TestReadManagedBookRows asserts the directory-scoped book-row discovery used
// for import detection, membership and highlight-volume filtering.
func TestReadManagedBookRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	ro, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	rows, err := readManagedBookRows(context.Background(), ro)
	if err != nil {
		t.Fatalf("readManagedBookRows: %v", err)
	}
	// The pristine fixture has exactly one book row under .kobo/readeck
	// (Book A); Book B lives under /books and must not be picked up.
	if len(rows) != 1 {
		t.Fatalf("managed book rows = %d, want 1", len(rows))
	}
	br, ok := rows[fixtureFileA]
	if !ok || br.ContentID != fixtureVolA {
		t.Fatalf("managed row = %+v (ok=%v)", br, ok)
	}
	if br.DateCreated != "2026-09-01T08:00:00Z" {
		t.Errorf("DateCreated = %q", br.DateCreated)
	}
	// The fixture leaves ___SyncTime NULL (Nickel sets it on import).
	if br.SyncTime != "" {
		t.Errorf("SyncTime = %q, want empty in pristine fixture", br.SyncTime)
	}
}
