package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// This file implements the two JSON sidecars kept in the data directory
// (the parent of the config file):
//
//   - index.json  — per-kepub-file state: what we downloaded, the server
//     etag it corresponds to, and how far it made it into Nickel's library
//     (downloaded → imported → removed).
//   - ledger.json — per device Bookmark row the last known server outcome,
//     keyed by bookmark row id. Rows whose device DateModified matches the
//     ledger value are not re-sent.
//
// Both are written atomically (temp file + rename) so a crash mid-write
// cannot corrupt them; unreadable/corrupt files are moved aside rather than
// failing the run.

// ---------------------------------------------------------------------------
// index.json

// fileState is the lifecycle of one kepub on the device.
type fileState string

const (
	stateDownloaded fileState = "downloaded" // file on disk, Nickel import not (yet) observed
	stateImported   fileState = "imported"   // content row observed; date-added columns ensured
	stateRemoved    fileState = "removed"    // server asked us to remove it (tombstone)
)

type indexEntry struct {
	Filename   string    `json:"filename"`
	BookmarkID string    `json:"bookmark_id"`
	Title      string    `json:"title"`
	ETag       string    `json:"etag"`
	Updated    string    `json:"updated"`
	Created    string    `json:"created"`
	State      fileState `json:"state"`
	RemovedAt  string    `json:"removed_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

type indexDoc struct {
	Version int                   `json:"version"`
	Files   map[string]indexEntry `json:"files"` // key: kepub file name
}

type indexStore struct {
	path string
	doc  indexDoc
}

func newIndexStore(path string) *indexStore {
	return &indexStore{path: path, doc: indexDoc{Version: 1, Files: map[string]indexEntry{}}}
}

// loadIndex reads the index, treating a missing file as an empty index and a
// corrupt file as an empty index plus an error the caller should log (the
// corrupt copy is preserved next to the file for forensics).
func loadIndex(path string) (*indexStore, error) {
	s := newIndexStore(path)
	err := readJSON(path, &s.doc)
	if err == nil {
		if s.doc.Files == nil {
			s.doc.Files = map[string]indexEntry{}
		}
		return s, nil
	}
	if os.IsNotExist(err) {
		return s, nil
	}
	// Corrupt or unreadable: keep a copy, restart empty.
	_ = os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102T150405"))
	return s, fmt.Errorf("index %s unreadable, starting empty: %v", path, err)
}

func (s *indexStore) get(filename string) (indexEntry, bool) {
	e, ok := s.doc.Files[filename]
	return e, ok
}

func (s *indexStore) set(e indexEntry) { s.doc.Files[e.Filename] = e }

func (s *indexStore) delete(filename string) { delete(s.doc.Files, filename) }

// entries returns all entries sorted by filename (deterministic iteration).
func (s *indexStore) entries() []indexEntry {
	out := make([]indexEntry, 0, len(s.doc.Files))
	for _, e := range s.doc.Files {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// managed returns the entries that represent kepubs we currently manage
// (downloaded or imported, i.e. not removed).
func (s *indexStore) managed() []indexEntry {
	var out []indexEntry
	for _, e := range s.entries() {
		if e.State != stateRemoved {
			out = append(out, e)
		}
	}
	return out
}

// pending returns entries whose file was downloaded but whose import has not
// been observed yet (date-added columns not yet ensured).
func (s *indexStore) pending() []indexEntry {
	var out []indexEntry
	for _, e := range s.entries() {
		if e.State == stateDownloaded && e.ETag != "" {
			out = append(out, e)
		}
	}
	return out
}

func (s *indexStore) save() error {
	return writeJSONAtomic(s.path, &s.doc)
}

// ---------------------------------------------------------------------------
// ledger.json

// ledgerEntry records the last definitive server outcome for one device
// Bookmark row. SentDateModified is the raw device DateModified value (or
// the DateCreated fallback) that was sent; a row is re-sent only when its
// current device value differs from this, or when there is no entry at all.
// Error/skipped-with-reason outcomes are also remembered (as LastError) so a
// permanently broken row is not retried forever, while server-side "error"
// outcomes are NOT recorded, leaving the row for the next run.
type ledgerEntry struct {
	BookmarkRowID      string `json:"bookmark_row_id"`
	BookmarkID         string `json:"bookmark_id"` // Readeck bookmark (article)
	VolumeID           string `json:"volume_id"`
	DeviceDateModified string `json:"date_modified"` // effective device DateModified sent
	Status             string `json:"status"`        // created|updated|unchanged|skipped
	AnnotationID       string `json:"annotation_id,omitempty"`
	LastError          string `json:"last_error,omitempty"`
	SentAt             string `json:"sent_at"`
}

type ledgerDoc struct {
	Version int                    `json:"version"`
	Entries map[string]ledgerEntry `json:"entries"` // key: device Bookmark row id
}

type ledgerStore struct {
	path string
	doc  ledgerDoc
}

func newLedgerStore(path string) *ledgerStore {
	return &ledgerStore{path: path, doc: ledgerDoc{Version: 1, Entries: map[string]ledgerEntry{}}}
}

func loadLedger(path string) (*ledgerStore, error) {
	s := newLedgerStore(path)
	err := readJSON(path, &s.doc)
	if err == nil {
		if s.doc.Entries == nil {
			s.doc.Entries = map[string]ledgerEntry{}
		}
		return s, nil
	}
	if os.IsNotExist(err) {
		return s, nil
	}
	_ = os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102T150405"))
	return s, fmt.Errorf("ledger %s unreadable, starting empty: %v", path, err)
}

func (s *ledgerStore) get(rowID string) (ledgerEntry, bool) {
	e, ok := s.doc.Entries[rowID]
	return e, ok
}

func (s *ledgerStore) set(e ledgerEntry) { s.doc.Entries[e.BookmarkRowID] = e }

func (s *ledgerStore) count() int { return len(s.doc.Entries) }

func (s *ledgerStore) save() error { return writeJSONAtomic(s.path, &s.doc) }

// ---------------------------------------------------------------------------
// atomic JSON persistence

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
