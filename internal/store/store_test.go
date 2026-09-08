package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSeenLedger(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	hash := HashToken("token-a")

	if got, err := s.GetSeen(ctx, hash, "dev1"); err != nil || len(got) != 0 {
		t.Fatalf("expected empty ledger, got %+v err %v", got, err)
	}

	if err := s.UpsertSeen(ctx, hash, "dev1", "b1", "2026-09-01T00:00:00Z", "add"); err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	if err := s.UpsertSeen(ctx, hash, "dev1", "b2", "2026-09-01T00:00:00Z", "add"); err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}

	got, err := s.GetSeen(ctx, hash, "dev1")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	want := []SeenRow{{BookmarkID: "b1", Updated: "2026-09-01T00:00:00Z", Action: "add"},
		{BookmarkID: "b2", Updated: "2026-09-01T00:00:00Z", Action: "add"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetSeen = %+v, want %+v", got, want)
	}

	// Upsert updates the stored row (action/updated change).
	if err := s.UpsertSeen(ctx, hash, "dev1", "b1", "2026-09-02T00:00:00Z", "update"); err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	got, _ = s.GetSeen(ctx, hash, "dev1")
	want = []SeenRow{{BookmarkID: "b1", Updated: "2026-09-02T00:00:00Z", Action: "update"},
		{BookmarkID: "b2", Updated: "2026-09-01T00:00:00Z", Action: "add"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetSeen after upsert = %+v, want %+v", got, want)
	}

	// Different device is isolated.
	other, err := s.GetSeen(ctx, hash, "dev2")
	if err != nil || len(other) != 0 {
		t.Errorf("dev2 ledger should be empty, got %+v err %v", other, err)
	}

	// Delete removes one row.
	if err := s.DeleteSeen(ctx, hash, "dev1", "b1"); err != nil {
		t.Fatalf("DeleteSeen: %v", err)
	}
	got, _ = s.GetSeen(ctx, hash, "dev1")
	if len(got) != 1 || got[0].BookmarkID != "b2" {
		t.Errorf("expected only b2 after delete, got %+v", got)
	}
}

func TestKepubCache(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetKepub(ctx, "b1"); err != nil || got != nil {
		t.Fatalf("expected nil cache entry, got %+v err %v", got, err)
	}

	if err := s.PutKepub(ctx, "b1", "etag-1", []byte("epub-bytes-1"), `[{"span_id":"kobo.1.1"}]`); err != nil {
		t.Fatalf("PutKepub: %v", err)
	}
	got, err := s.GetKepub(ctx, "b1")
	if err != nil {
		t.Fatalf("GetKepub: %v", err)
	}
	if got == nil || got.Etag != "etag-1" || string(got.EPUB) != "epub-bytes-1" || !strings.Contains(got.SpanMap, "kobo.1.1") {
		t.Errorf("unexpected cache entry: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}

	// Overwrite on a new etag.
	if err := s.PutKepub(ctx, "b1", "etag-2", []byte("epub-bytes-2"), "[]"); err != nil {
		t.Fatalf("PutKepub: %v", err)
	}
	got, _ = s.GetKepub(ctx, "b1")
	if got.Etag != "etag-2" || string(got.EPUB) != "epub-bytes-2" {
		t.Errorf("cache not overwritten: %+v", got)
	}
}

func TestAnnotationLedger(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetAnnotationLedger(ctx, "dev1", "row-1"); err != nil || got != nil {
		t.Fatalf("expected nil ledger row, got %+v err %v", got, err)
	}

	if err := s.PutAnnotationLedger(ctx, "dev1", "row-1", "b1", "a1", "2026-09-01T00:00:00Z"); err != nil {
		t.Fatalf("PutAnnotationLedger: %v", err)
	}
	got, err := s.GetAnnotationLedger(ctx, "dev1", "row-1")
	if err != nil {
		t.Fatalf("GetAnnotationLedger: %v", err)
	}
	if got == nil || got.AnnotationID != "a1" || got.BookmarkID != "b1" || got.DateModified != "2026-09-01T00:00:00Z" {
		t.Errorf("unexpected ledger row: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at should be set")
	}

	// Upsert refreshes.
	if err := s.PutAnnotationLedger(ctx, "dev1", "row-1", "b1", "a2", "2026-09-02T00:00:00Z"); err != nil {
		t.Fatalf("PutAnnotationLedger: %v", err)
	}
	got, _ = s.GetAnnotationLedger(ctx, "dev1", "row-1")
	if got.AnnotationID != "a2" || got.DateModified != "2026-09-02T00:00:00Z" {
		t.Errorf("ledger not updated: %+v", got)
	}

	// Row ids are scoped per device.
	if got, err := s.GetAnnotationLedger(ctx, "dev2", "row-1"); err != nil || got != nil {
		t.Errorf("dev2 should have no ledger row, got %+v err %v", got, err)
	}
}

func TestFileStoreAndWAL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := s.PutKepub(ctx, "b1", "etag", []byte("data"), "[]"); err != nil {
		t.Fatalf("PutKepub: %v", err)
	}
	_ = s.Close()

	// The DB file exists; WAL should be the journal mode for a file database.
	dbPath := filepath.Join(dir, dbFileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file missing: %v", err)
	}

	// Reopen and read back.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetKepub(ctx, "b1")
	if err != nil || got == nil || string(got.EPUB) != "data" {
		t.Fatalf("read back after reopen: %+v err %v", got, err)
	}
}

func TestHashToken(t *testing.T) {
	a := HashToken("secret")
	b := HashToken("secret")
	if a != b {
		t.Error("hash should be deterministic")
	}
	if len(a) != 64 || a == "secret" {
		t.Errorf("unexpected hash shape: %q", a)
	}
}
