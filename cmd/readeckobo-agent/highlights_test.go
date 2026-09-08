package main

import (
	"context"
	"database/sql"
	"testing"
)

// The fixture's canonical (un-rewritten) names: the fixture DB keeps its
// original file:///mnt/onboard/... paths, so extraction is exercised against
// exactly what tools/kobofixdb generates (see docs/research/kobo-fixture-db.md).
const (
	fixtureFileA = "Readeck Article One.kepub.epub"
	fixtureVolA  = "file:///mnt/onboard/.kobo/readeck/" + fixtureFileA
)

func managedIndexA() *indexStore {
	idx := newIndexStore("")
	idx.set(indexEntry{
		Filename:   fixtureFileA,
		BookmarkID: "bmA1",
		Title:      "Readeck Article One",
		ETag:       "e1",
		State:      stateDownloaded,
	})
	return idx
}

// TestHighlightExtractionFromFixture runs the canonical extraction against a
// COPY of the generated fixture DB and asserts every field of the 3 Book-A
// rows (2 highlights + 1 note), ordered by DateCreated.
func TestHighlightExtractionFromFixture(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)

	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open fixture copy: %v", err)
	}
	defer db.Close()

	highlights, err := scanHighlights(context.Background(), db)
	if err != nil {
		t.Fatalf("scanHighlights: %v", err)
	}

	// Keep only rows for the managed volume (the Book-A fixture file).
	idx := managedIndexA()
	var mine []deviceHighlight
	for _, h := range highlights {
		if _, ok := managedFilename(h.VolumeID, idx); ok {
			mine = append(mine, h)
		}
	}
	if len(mine) != 3 {
		t.Fatalf("managed rows = %d, want 3 (2 highlights + 1 note); all=%d", len(mine), len(highlights))
	}

	// Canonical SQL orders by DateCreated.
	wantOrder := []string{rowHlA1, rowHlA2, rowNoteA1}
	for i, h := range mine {
		if h.RowID != wantOrder[i] {
			t.Errorf("row %d = %s, want %s", i, h.RowID, wantOrder[i])
		}
	}

	hl1 := mine[0]
	if hl1.VolumeID != fixtureVolA || hl1.Type != "highlight" {
		t.Errorf("hlA1 volume/type wrong: %+v", hl1)
	}
	if hl1.StartContainerPath.String != `span#kobo\.5\.2` || hl1.EndContainerPath.String != `span#kobo\.5\.2` {
		t.Errorf("hlA1 paths wrong: %+v", hl1)
	}
	if hl1.StartOffset.String != "0" || hl1.EndOffset.String != "58" {
		t.Errorf("hlA1 offsets wrong: %+v", hl1)
	}
	if hl1.DateCreated.String != "2026-09-02T09:15:00Z" || hl1.DateModified.String != "2026-09-02T09:16:00Z" {
		t.Errorf("hlA1 dates wrong: %+v", hl1)
	}

	hl2 := mine[1]
	if hl2.StartContainerPath.String != `span#kobo\.3\.1` || hl2.EndContainerPath.String != `span#kobo\.3\.2` {
		t.Errorf("hlA2 multi-span paths wrong: %+v", hl2)
	}

	note1 := mine[2]
	if note1.Type != "note" || note1.Annotation.String != "Need to double-check this claim before publishing." {
		t.Errorf("noteA1 wrong: %+v", note1)
	}

	// Wire-item conversion: offsets become integers, empty annotation → "".
	item, ok := buildUploadItem(mine[0], "bmA1", effectiveModified(mine[0]))
	if !ok {
		t.Fatal("buildUploadItem(hlA1) failed")
	}
	if item.BookmarkID != "bmA1" || item.BookmarkRowID != rowHlA1 ||
		item.StartOffset != 0 || item.EndOffset != 58 ||
		item.StartPath != `span#kobo\.5\.2` || item.Type != "highlight" ||
		item.DateCreated != "2026-09-02T09:15:00Z" || item.DateModified != "2026-09-02T09:16:00Z" {
		t.Errorf("upload item wrong: %+v", item)
	}
}

// TestHighlightScanFilters verifies the exclusion rules on a mutable fixture
// copy: dogear rows, Hidden rows, empty-text rows and rows for other files in
// the managed directory must not come back, while a row without DateModified
// falls back to DateCreated for change tracking.
func TestHighlightScanFilters(t *testing.T) {
	dir := t.TempDir()
	dbPath := copyFixtureDB(t, dir)
	db := openFixtureRW(t, dbPath)

	insertBookmarkTestRow(t, db, "extra-dogear", fixtureVolA, `span#kobo\.1\.1`, "0", "dogear", "a dogear",
		"2026-09-04T00:00:00Z", "2026-09-04T00:00:00Z", "false")
	insertBookmarkTestRow(t, db, "extra-hidden", fixtureVolA, `span#kobo\.1\.2`, "0", "highlight", "hidden text",
		"2026-09-04T00:01:00Z", "2026-09-04T00:01:00Z", "true")
	insertBookmarkTestRow(t, db, "extra-empty", fixtureVolA, `span#kobo\.1\.3`, "0", "highlight", "",
		"2026-09-04T00:02:00Z", "2026-09-04T00:02:00Z", "false")
	insertBookmarkTestRow(t, db, "extra-otherfile", "file:///mnt/onboard/.kobo/readeck/Another Article.kepub.epub",
		`span#kobo\.2\.1`, "0", "highlight", "not our managed file", "2026-09-04T00:03:00Z",
		"2026-09-04T00:03:00Z", "false")
	// No DateModified → effective falls back to DateCreated.
	insertBookmarkTestRow(t, db, "extra-nomod", fixtureVolA, `span#kobo\.1\.4`, "0", "highlight", "no mod date",
		"2026-09-04T00:04:00Z", "", "false")

	// Assert the SQL-level filters (dogear, hidden, empty text) directly.
	ro, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	highlights, err := scanHighlights(context.Background(), ro)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, h := range highlights {
		ids[h.RowID] = true
	}
	// dogear / hidden / empty-text rows are excluded at the SQL level; the
	// other-file row is still returned by the directory-scoped scan and is
	// dropped by the Go-level managed filter below.
	for _, banned := range []string{"extra-dogear", "extra-hidden", "extra-empty"} {
		if ids[banned] {
			t.Errorf("row %s survived the SQL scan filters", banned)
		}
	}
	if !ids["extra-otherfile"] {
		t.Errorf("extra-otherfile should survive the SQL scan (dropped later by the managed filter)")
	}
	if !ids["extra-nomod"] {
		t.Errorf("extra-nomod should have survived (valid highlight without DateModified)")
	}

	// Go-level managed filter drops the other-file row; the no-mod row is
	// kept with DateCreated as the effective change-tracking value.
	idx := managedIndexA()
	byID := map[string]deviceHighlight{}
	for _, h := range highlights {
		if _, ok := managedFilename(h.VolumeID, idx); ok {
			byID[h.RowID] = h
		}
	}
	if _, ok := byID["extra-otherfile"]; ok {
		t.Errorf("other-file row not dropped by managed filter")
	}
	if got := effectiveModified(byID["extra-nomod"]); got != "2026-09-04T00:04:00Z" {
		t.Errorf("effectiveModified = %q, want DateCreated fallback", got)
	}
}

func insertBookmarkTestRow(t *testing.T, db *sql.DB, id, volumeID, startPath, startOff, typ, text, dateCreated, dateModified, hidden string) {
	t.Helper()
	if dateModified == "" {
		dateModified = "NULL"
	} else {
		dateModified = "'" + dateModified + "'"
	}
	execDB(t, db, `INSERT INTO Bookmark
		(BookmarkID, VolumeID, ContentID, StartContainerPath, StartContainerChildIndex,
		 StartOffset, EndContainerPath, EndContainerChildIndex, EndOffset, Text, Annotation,
		 ExtraAnnotationData, DateCreated, ChapterProgress, Hidden, Version, DateModified,
		 Creator, UUID, UserID, SyncTime, Published, ContextString, Type)
		VALUES (?, ?, ?, ?, '-99', ?, ?, '-99', ?, ?, '', NULL, ?, '0.5', ?, NULL, `+dateModified+`,
			NULL, NULL, '', NULL, 'false', NULL, ?)`,
		id, volumeID, volumeID+"!!OEBPS/xhtml/ch001.xhtml", startPath, startOff, startPath, startOff,
		text, dateCreated, hidden, typ)
}

// TestBuildUploadItemOffsetFailure asserts that a row with unparseable
// offsets is rejected by item building (the caller skips it with a reason).
func TestBuildUploadItemOffsetFailure(t *testing.T) {
	h := deviceHighlight{
		RowID:       "broken-row",
		VolumeID:    fixtureVolA,
		Type:        "highlight",
		Text:        "x",
		StartOffset: sql.NullString{String: "not-a-number", Valid: true},
		EndOffset:   sql.NullString{String: "12", Valid: true},
	}
	if _, ok := buildUploadItem(h, "bmA1", "2026-01-01T00:00:00Z"); ok {
		t.Fatal("buildUploadItem should have rejected unparseable offsets")
	}
}
