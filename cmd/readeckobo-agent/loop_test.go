package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// articleA/B mirror the fixture books after path rewriting (see
// testhelpers_test.go): book A is "Readeck Article One" (3 highlights + 1
// note on device), book B is "Project Hail Mary" (1 highlight).
// updated is the content version (etag driver); created is the stable Readeck
// date-added that drives the Kobo sort. They are deliberately distinct so the
// tests prove the device uses created, not updated.
func articleA(etag, updated, created string) Article {
	return Article{
		BookmarkID: bookAID,
		Title:      "Readeck Article One",
		Author:     "Readeck",
		URL:        "",
		ETag:       etag,
		Action:     "update",
		Updated:    updated,
		Created:    created,
	}
}

func articleB(etag, updated, created string) Article {
	return Article{
		BookmarkID: bookBID,
		Title:      "Project Hail Mary",
		Author:     "Andy Weir",
		URL:        "",
		ETag:       etag,
		Action:     "add",
		Updated:    updated,
		Created:    created,
	}
}

const (
	fileA = "Readeck Article One-bmA1.kepub.epub"
	fileB = "Project Hail Mary-bmB1.kepub.epub"

	rowHlA1   = "9a3f7b2e-1111-4aaa-aaaa-000000000001"
	rowHlA2   = "9a3f7b2e-2222-4bbb-bbbb-000000000002"
	rowNoteA1 = "9a3f7b2e-3333-4ccc-cccc-000000000003"
	rowHlB1   = "9a3f7b2e-4444-4ddd-dddd-000000000004"
)

func fullArticle(a Article, srv string) Article {
	a.URL = srv + "/api/kepub/" + a.BookmarkID
	return a
}

// TestSyncLoopEndToEnd drives the complete agent loop against a fixture-based
// device DB across four passes: import + highlight upload, content update
// (etag change must NOT move the date-added back to updated), removal, and a
// second import with its own highlight. It runs with --no-rescan semantics
// (Nickel is simulated by pre-seeded content rows, which is exactly the
// contract: the agent never inserts content rows itself).
func TestSyncLoopEndToEnd(t *testing.T) {
	env := newLoopEnv(t)

	a1 := fullArticle(articleA("etag-1", "2026-09-07T10:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL)
	env.setKepub(bookAID, "KEPUB-A-V1")
	env.setState(a1)

	// --- pass 1: import A, ensure date-added from created, create collection, upload 3 ---
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	kepubPath := filepath.Join(env.onboard, ".kobo", "readeck", fileA)
	data, err := os.ReadFile(kepubPath)
	if err != nil {
		t.Fatalf("kepub A not on disk: %v", err)
	}
	if string(data) != "KEPUB-A-V1" {
		t.Fatalf("kepub A content = %q", data)
	}

	db := openFixtureRW(t, env.koboDB)
	// Date-added columns converge to article.created (not updated).
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("DateCreated = %q, want article created", got)
	}
	if got := scanOne(t, db, `SELECT ___SyncTime FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("___SyncTime = %q, want article created", got)
	}
	if got := scanOne(t, db, `SELECT DateModified FROM ShelfContent WHERE ShelfName = 'Readeck' AND ContentId = ?`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("ShelfContent.DateModified = %q, want article created", got)
	}
	// Shelf + membership created by the agent (we wiped them in setup).
	if got := scanOne(t, db, `SELECT count(*) FROM Shelf WHERE Name = 'Readeck' AND _IsDeleted = 'false'`); got != "1" {
		t.Fatalf("Readeck shelf count = %s", got)
	}
	if got := scanOne(t, db, `SELECT count(*) FROM ShelfContent WHERE ShelfName = 'Readeck' AND ContentId = ? AND _IsDeleted = 'false'`,
		env.volumeID(fileA)); got != "1" {
		t.Fatalf("shelf membership for A = %s", got)
	}

	// Highlights uploaded: one POST with the 3 Book-A rows.
	if got := env.postCount(); got != 1 {
		t.Fatalf("annotation POSTs = %d, want 1", got)
	}
	post := env.lastPost()
	if post.Device != testSerial {
		t.Errorf("POST device = %q", post.Device)
	}
	if len(post.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(post.Items))
	}
	byRow := map[string]uploadItem{}
	for _, it := range post.Items {
		byRow[it.BookmarkRowID] = it
	}
	hl1, ok := byRow[rowHlA1]
	if !ok {
		t.Fatalf("no item for %s", rowHlA1)
	}
	if hl1.BookmarkID != bookAID || hl1.Text != "Readeck lets you keep highlights of articles you read on your e-reader." {
		t.Errorf("hlA1 fields wrong: %+v", hl1)
	}
	if hl1.StartPath != `span#kobo\.5\.2` || hl1.StartOffset != 0 || hl1.EndOffset != 58 {
		t.Errorf("hlA1 offsets wrong: %+v", hl1)
	}
	if hl1.Type != "highlight" || hl1.DateCreated != "2026-09-02T09:15:00Z" || hl1.DateModified != "2026-09-02T09:16:00Z" {
		t.Errorf("hlA1 dates wrong: %+v", hl1)
	}
	if hl1.Annotation != "" {
		t.Errorf("hlA1 annotation = %q, want empty", hl1.Annotation)
	}
	note1 := byRow[rowNoteA1]
	if note1.Type != "note" || note1.Annotation != "Need to double-check this claim before publishing." {
		t.Errorf("noteA1 wrong: %+v", note1)
	}

	// Ledger sidecar persisted with the definitive outcomes.
	ledgerData, err := os.ReadFile(filepath.Join(env.dataDir, "ledger.json"))
	if err != nil {
		t.Fatalf("ledger.json: %v", err)
	}
	for _, rowID := range []string{rowHlA1, rowHlA2, rowNoteA1} {
		if !strings.Contains(string(ledgerData), rowID) {
			t.Errorf("ledger.json missing %s", rowID)
		}
	}

	// --- pass 2: same article, new etag (updated moves, created stable) →
	// re-download, but the date-added must NOT move and nothing new is uploaded ---
	env.setKepub(bookAID, "KEPUB-A-V2")
	env.setState(fullArticle(articleA("etag-2", "2026-09-08T09:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL))
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	data, err = os.ReadFile(kepubPath)
	if err != nil || string(data) != "KEPUB-A-V2" {
		t.Fatalf("pass 2: kepub not updated: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("pass 2: DateCreated moved to %q, want stable created", got)
	}
	if got := scanOne(t, db, `SELECT ___SyncTime FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Fatalf("pass 2: ___SyncTime moved to %q, want stable created", got)
	}
	if got := env.postCount(); got != 1 {
		t.Fatalf("pass 2: annotation POSTs = %d, want still 1", got)
	}

	// --- pass 3: removal → file deleted, tombstoned; nothing uploaded ---
	env.setState(Article{BookmarkID: bookAID, Title: "Readeck Article One", ETag: "etag-2", Action: "remove", Updated: "2026-09-08T10:00:00Z"})
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if _, err := os.Stat(kepubPath); !os.IsNotExist(err) {
		t.Fatalf("pass 3: kepub still present after removal")
	}
	idx := mustLoadIndex(t, env)
	if e, ok := idx.get(fileA); !ok || e.State != stateRemoved {
		t.Fatalf("pass 3: index entry = %+v (ok=%v), want removed tombstone", e, ok)
	}
	if got := env.postCount(); got != 1 {
		t.Fatalf("pass 3: annotation POSTs = %d, want still 1 (removed book must not upload)", got)
	}

	// --- pass 4: import B (fresh book, own highlight) ---
	env.setKepub(bookBID, "KEPUB-B-V1")
	env.setState(fullArticle(articleB("etag-b1", "2026-09-05T14:30:00Z", "2026-08-15T12:30:00Z"), env.server.URL))
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileB)); got != "2026-08-15T12:30:00Z" {
		t.Fatalf("pass 4: B DateCreated = %q, want article created", got)
	}
	if got := scanOne(t, db, `SELECT ___SyncTime FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileB)); got != "2026-08-15T12:30:00Z" {
		t.Fatalf("pass 4: B ___SyncTime = %q, want article created", got)
	}
	if got := scanOne(t, db, `SELECT count(*) FROM ShelfContent WHERE ShelfName = 'Readeck' AND ContentId = ?`,
		env.volumeID(fileB)); got != "1" {
		t.Fatalf("pass 4: B membership = %s", got)
	}
	if got := env.postCount(); got != 2 {
		t.Fatalf("pass 4: annotation POSTs = %d, want 2", got)
	}
	last := env.lastPost()
	if len(last.Items) != 1 || last.Items[0].BookmarkRowID != rowHlB1 {
		t.Fatalf("pass 4: last POST = %+v, want hlB1 only", last.Items)
	}
}

func mustLoadIndex(t *testing.T, env *loopEnv) *indexStore {
	t.Helper()
	idx, err := loadIndex(filepath.Join(env.dataDir, "index.json"))
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	return idx
}

// TestMaintainCollectionSelfHeals covers the date-added migration: a book
// imported before the fix carries the old wrong values (DateCreated = updated
// at import time, ___SyncTime = Nickel's import time, shelf DateModified =
// sync time). The next pass must converge all three to article.created even
// though the index already says "imported".
func TestMaintainCollectionSelfHeals(t *testing.T) {
	env := newLoopEnv(t)
	env.setKepub(bookAID, "KEPUB-A-V1")
	env.setState(fullArticle(articleA("etag-1", "2026-09-07T10:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL))
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	db := openFixtureRW(t, env.koboDB)
	// Corrupt the row to the pre-fix shape: updated-based DateCreated, import
	// time ___SyncTime, sync-time shelf membership.
	execDB(t, db, `UPDATE content SET DateCreated = ?, ___SyncTime = ? WHERE ContentID = ? AND VolumeIndex = -1`,
		"2026-09-07T10:00:00Z", "2026-09-10T12:00:00Z", env.volumeID(fileA))
	execDB(t, db, `UPDATE ShelfContent SET DateModified = ? WHERE ShelfName = 'Readeck' AND ContentId = ?`,
		"2026-09-10T12:00:00Z", env.volumeID(fileA))
	// Simulate an old index without the created field (pre-fix sidecar).
	idx := mustLoadIndex(t, env)
	if e, ok := idx.get(fileA); ok {
		e.Created = ""
		e.State = stateImported
		idx.set(e)
		if err := idx.save(); err != nil {
			t.Fatalf("index save: %v", err)
		}
	}

	// Pass 2 with the same feed must heal all three columns and backfill the
	// index, without re-downloading (same etag).
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := scanOne(t, db, `SELECT DateCreated FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Errorf("healed DateCreated = %q, want created", got)
	}
	if got := scanOne(t, db, `SELECT ___SyncTime FROM content WHERE ContentID = ? AND VolumeIndex = -1`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Errorf("healed ___SyncTime = %q, want created", got)
	}
	if got := scanOne(t, db, `SELECT DateModified FROM ShelfContent WHERE ShelfName = 'Readeck' AND ContentId = ?`,
		env.volumeID(fileA)); got != "2026-09-01T08:00:00Z" {
		t.Errorf("healed ShelfContent.DateModified = %q, want created", got)
	}
	healed := mustLoadIndex(t, env)
	if e, ok := healed.get(fileA); !ok || e.Created == "" {
		t.Errorf("index Created not backfilled: %+v (ok=%v)", e, ok)
	} else if e.State != stateImported {
		t.Errorf("index state = %q, want imported", e.State)
	}
}

// TestDesiredCreatedFallback covers the timestamp resolution: feed created
// wins, else index created, else legacy updated (old servers).
func TestDesiredCreatedFallback(t *testing.T) {
	feed := map[string]Article{
		"b1": {BookmarkID: "b1", Created: "2026-09-01T08:00:00Z", Updated: "2026-09-07T10:00:00Z"},
	}
	if got := desiredCreated(indexEntry{BookmarkID: "b1", Created: "2026-08-01T00:00:00Z"}, feed); got != "2026-09-01T08:00:00Z" {
		t.Errorf("feed should win, got %q", got)
	}
	if got := desiredCreated(indexEntry{BookmarkID: "b9", Created: "2026-08-01T00:00:00Z", Updated: "2026-09-07T00:00:00Z"}, feed); got != "2026-08-01T00:00:00Z" {
		t.Errorf("index created should win when absent from feed, got %q", got)
	}
	if got := desiredCreated(indexEntry{BookmarkID: "b9", Updated: "2026-09-07T00:00:00Z"}, feed); got != "2026-09-07T00:00:00Z" {
		t.Errorf("legacy updated fallback, got %q", got)
	}
}

// TestSyncLoopMissedRemoveSweep covers the once-only remove emission: the
// server sends each remove exactly once and then drops its ledger row, so a
// device that missed that emission (offline, or the remove was consumed by a
// different client sharing the device id) never sees it again. The feed
// still carries every live bookmark, so a managed file whose bookmark is
// absent from the feed must be swept as if removed (this is what finally
// deletes video kepubs synced before the server excluded videos).
func TestSyncLoopMissedRemoveSweep(t *testing.T) {
	env := newLoopEnv(t)
	env.setKepub(bookAID, "KEPUB-A-V1")
	env.setKepub(bookBID, "KEPUB-B-V1")

	// Pass 1: both articles live → both files on disk and managed.
	env.setState(
		fullArticle(articleA("etag-1", "2026-09-07T10:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL),
		fullArticle(articleB("etag-b1", "2026-09-05T14:30:00Z", "2026-08-15T12:30:00Z"), env.server.URL),
	)
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	pathA := filepath.Join(env.onboard, ".kobo", "readeck", fileA)
	if _, err := os.Stat(pathA); err != nil {
		t.Fatalf("pass 1: kepub A not on disk: %v", err)
	}

	// Pass 2: A vanishes from the feed with NO explicit remove (the missed
	// emission). The sweep must delete the file and tombstone the index.
	env.setState(fullArticle(articleB("etag-b1", "2026-09-05T14:30:00Z", "2026-08-15T12:30:00Z"), env.server.URL))
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatalf("pass 2: kepub A still present without a feed entry")
	}
	idx := mustLoadIndex(t, env)
	if e, ok := idx.get(fileA); !ok || e.State != stateRemoved {
		t.Fatalf("pass 2: index entry A = %+v (ok=%v), want removed tombstone", e, ok)
	}
	// B is untouched: still managed, still on disk.
	if e, ok := idx.get(fileB); !ok || e.State == stateRemoved {
		t.Fatalf("pass 2: index entry B = %+v (ok=%v), want it still managed", e, ok)
	}
	pathB := filepath.Join(env.onboard, ".kobo", "readeck", fileB)
	if _, err := os.Stat(pathB); err != nil {
		t.Fatalf("pass 2: kepub B missing: %v", err)
	}

	// Pass 3: steady state — the tombstone must not cause another rescan
	// trigger: nothing changes on disk.
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	idx = mustLoadIndex(t, env)
	if e, ok := idx.get(fileA); !ok || e.State != stateRemoved {
		t.Fatalf("pass 3: index entry A = %+v (ok=%v), want tombstone to persist", e, ok)
	}
}

// TestSyncLoopServerOutcomes covers the ledger semantics for the mixed
// per-row statuses the server can return: definitive statuses are recorded,
// an error status leaves the row for the next pass.
func TestSyncLoopServerOutcomes(t *testing.T) {
	env := newLoopEnv(t)
	env.setKepub(bookAID, "KEPUB-A")
	env.setState(fullArticle(articleA("etag-1", "2026-09-07T10:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL))
	env.resultFn = func(rowID string) annotationResult {
		switch rowID {
		case rowHlA1:
			return annotationResult{Status: "created", AnnotationID: "annotation-1"}
		case rowHlA2:
			return annotationResult{Status: "error", Error: "overlap conflict"}
		case rowNoteA1:
			return annotationResult{Status: "skipped", Error: "duplicate"}
		}
		return annotationResult{Status: "created"}
	}
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	led := mustLoadLedger(t, env)
	if e, ok := led.get(rowHlA1); !ok || e.Status != "created" || e.AnnotationID != "annotation-1" {
		t.Errorf("hlA1 ledger = %+v", e)
	}
	if _, ok := led.get(rowHlA2); ok {
		t.Errorf("hlA2 must NOT be in the ledger after an error status (retry next pass)")
	}
	if e, ok := led.get(rowNoteA1); !ok || e.Status != "skipped" || e.LastError != "duplicate" {
		t.Errorf("noteA1 ledger = %+v (want skipped-with-reason)", e)
	}

	// Pass 2: only hlA2 is re-sent.
	if err := env.runSync(t); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if got := env.postCount(); got != 2 {
		t.Fatalf("POSTs = %d, want 2", got)
	}
	second := env.lastPost()
	if len(second.Items) != 1 || second.Items[0].BookmarkRowID != rowHlA2 {
		t.Fatalf("second POST = %+v, want hlA2 only", second.Items)
	}
}

func mustLoadLedger(t *testing.T, env *loopEnv) *ledgerStore {
	t.Helper()
	led, err := loadLedger(filepath.Join(env.dataDir, "ledger.json"))
	if err != nil {
		t.Fatalf("loadLedger: %v", err)
	}
	return led
}

// TestCLIOnce runs the real CLI entry point (flag parsing → config file →
// single sync pass) against the loop env.
func TestCLIOnce(t *testing.T) {
	env := newLoopEnv(t)
	env.writeConfig(nil)
	env.setKepub(bookAID, "KEPUB-A")
	env.setState(fullArticle(articleA("etag-1", "2026-09-07T10:00:00Z", "2026-09-01T08:00:00Z"), env.server.URL))

	args := []string{
		"--once",
		"--config", filepath.Join(env.dataDir, "config"),
		"--kobo-db", env.koboDB,
		"--onboard", env.onboard,
		"--no-rescan",
	}
	code := runCLI(context.Background(), args, io.Discard)
	if code != 0 {
		t.Fatalf("runCLI exit code = %d", code)
	}
	if env.postCount() != 1 {
		t.Fatalf("expected one annotations POST, got %d", env.postCount())
	}
	if _, err := os.Stat(filepath.Join(env.onboard, ".kobo", "readeck", fileA)); err != nil {
		t.Fatalf("kepub missing after CLI run: %v", err)
	}
}

// TestSingleInstanceLock makes sure the flock actually excludes a second
// agent process.
func TestSingleInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	l1, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireInstanceLock(path); err == nil {
		t.Fatal("second lock acquisition succeeded; want failure")
	}
	l1.release()
	l2, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l2.release()
}
