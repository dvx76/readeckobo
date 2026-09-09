package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// util

func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Readeck Article One", "Readeck Article One"},
		{"../../etc/passwd", "etc passwd"}, // traversal: dots stripped, slashes → separators
		{"Title: With * Weird? Chars|", "Title With Weird Chars"},
		{"  spaced   out  ", "spaced out"},
		{"Trailing...   ", "Trailing"},
		{"", "Untitled"},
		{"...", "Untitled"},
		{"   ", "Untitled"},
		{"ünïcödé 📚 ok", "ünïcödé 📚 ok"},
	}
	for _, c := range cases {
		if got := sanitizeTitle(c.in); got != c.want {
			t.Errorf("sanitizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Length cap.
	long := strings.Repeat("a", 500)
	if got := sanitizeTitle(long); len([]rune(got)) != maxTitleRunes {
		t.Errorf("cap: len=%d, want %d", len([]rune(got)), maxTitleRunes)
	}
}

func TestKepubFilename(t *testing.T) {
	cases := []struct {
		title, id, want string
	}{
		{"Readeck Article One", "bmA1", "Readeck Article One-bmA1.kepub.epub"},
		{"Trailing.", "bmA1", "Trailing-bmA1.kepub.epub"},
		{"Bad/Name: ok", "abcDEF-123_4", "Bad Name ok-abcDEF-123_4.kepub.epub"},
		{"Untitled-less", "", "Untitled-less.kepub.epub"}, // no id → no suffix
		{"", "bmX", "Untitled-bmX.kepub.epub"},
	}
	for _, c := range cases {
		if got := kepubFilename(c.title, c.id); got != c.want {
			t.Errorf("kepubFilename(%q,%q) = %q, want %q", c.title, c.id, got, c.want)
		}
	}
}

func TestVolumeAndFilenameHelpers(t *testing.T) {
	onboard := "/mnt/onboard"
	if got := volumeIDFor(onboard, "A-bm1.kepub.epub"); got != "file:///mnt/onboard/.kobo/readeck/A-bm1.kepub.epub" {
		t.Errorf("volumeIDFor = %q", got)
	}
	cases := []struct{ vid, want string }{
		{"file:///mnt/onboard/.kobo/readeck/A-bm1.kepub.epub", "A-bm1.kepub.epub"},
		{"file:///mnt/onboard/.kobo/readeck/A.kepub.epub!!OEBPS/xhtml/ch001.xhtml", "A.kepub.epub"},
	}
	for _, c := range cases {
		if got := filenameOfVolumeID(c.vid); got != c.want {
			t.Errorf("filenameOfVolumeID(%q) = %q, want %q", c.vid, got, c.want)
		}
	}
}

func TestTimeHelpers(t *testing.T) {
	ok := []string{
		"2026-09-02T09:15:00Z",
		"2026-09-02T09:15:00+02:00",
		"2026-09-02T09:15:00.123Z",
		"2026-09-02 09:15:00",
		"2026-09-02T09:15:00",
	}
	for _, s := range ok {
		ts, err := parseKoboTime(s)
		if err != nil {
			t.Errorf("parseKoboTime(%q): %v", s, err)
			continue
		}
		if got := formatKoboTime(ts); !strings.HasSuffix(got, "Z") {
			t.Errorf("formatKoboTime(%q) = %q, want Z suffix", s, got)
		}
	}
	if _, err := parseKoboTime(""); !errors.Is(err, errEmptyTime) {
		t.Errorf("empty parse err = %v", err)
	}
	if _, err := parseKoboTime("not-a-date"); !errors.Is(err, errInvalidTime) {
		t.Errorf("bad parse err = %v", err)
	}
}

func TestParseFlexBool(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", "yes", "on"} {
		if v, err := parseFlexBool(s); err != nil || !v {
			t.Errorf("parseFlexBool(%q) = %v,%v want true", s, v, err)
		}
	}
	for _, s := range []string{"0", "false", "no", "off", ""} {
		if v, err := parseFlexBool(s); err != nil || v {
			t.Errorf("parseFlexBool(%q) = %v,%v want false", s, v, err)
		}
	}
	if _, err := parseFlexBool("maybe"); err == nil {
		t.Error("parseFlexBool(maybe) should error")
	}
}

// ---------------------------------------------------------------------------
// config

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := strings.Join([]string{
		"# readeckobo agent",
		"",
		"SERVER_URL = http://example.com:8080/",
		"TOKEN=sekret",
		"DEVICE_SERIAL = 12345",
		"COLLECTION=My Shelf",
		"ARCHIVE_ON_FINISHED=yes",
		"QNDB_PATH=/custom/qndb",
		"QNDB_ARGS=-t 1000 -s fssFinished -m n3fssSyncOnboard",
		"HTTP_TIMEOUT=5s",
		"UNKNOWN_KEY=ignored",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ServerURL != "http://example.com:8080" { // trailing slash trimmed
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.Token != "sekret" || cfg.DeviceSerial != "12345" || cfg.Collection != "My Shelf" {
		t.Errorf("basic fields wrong: %+v", cfg)
	}
	if !cfg.ArchiveOnFinished {
		t.Error("ArchiveOnFinished should be true")
	}
	if cfg.QNDBPath != "/custom/qndb" || strings.Join(cfg.QNDBArgs, " ") != "-t 1000 -s fssFinished -m n3fssSyncOnboard" {
		t.Errorf("qndb override wrong: %+v", cfg)
	}
	if cfg.HTTPTimeout != 5*time.Second {
		t.Errorf("HTTPTimeout = %v", cfg.HTTPTimeout)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "UNKNOWN_KEY") {
		t.Errorf("Warnings = %v", cfg.Warnings)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	os.WriteFile(path, []byte("SERVER_URL=http://x\nTOKEN=t\n"), 0o644)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collection != defaultCollection || cfg.DeviceSerial != "unknown" ||
		cfg.QNDBPath != defaultQNDBPath || cfg.HTTPTimeout != defaultHTTPTo ||
		strings.Join(cfg.QNDBArgs, " ") != defaultQNDBArgs {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if _, err := loadConfig(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file should error")
	}
	if err := os.WriteFile(path, []byte("SERVER_URL=http://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "TOKEN") {
		t.Errorf("missing TOKEN err = %v", err)
	}
	os.WriteFile(path, []byte("TOKEN=t\n"), 0o644)
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "SERVER_URL") {
		t.Errorf("missing SERVER_URL err = %v", err)
	}
	os.WriteFile(path, []byte("SERVER_URL=not a url\nTOKEN=t\n"), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("bad URL should error")
	}
	os.WriteFile(path, []byte("SERVER_URL=http://x\nTOKEN=t\nARCHIVE_ON_FINISHED=banana\n"), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("bad bool should error")
	}
	os.WriteFile(path, []byte("SERVER_URL=http://x\nTOKEN=t\nno-equals-here\n"), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("line without '=' should error")
	}
}

// ---------------------------------------------------------------------------
// api client

func testServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// testHTTPClient returns a fresh client for the test server with a
// client-level deadline, mirroring how the agent applies HTTP_TIMEOUT to its
// shared http.Client (covers headers + response body reads).
func testHTTPClient(srv *httptest.Server, d time.Duration) *http.Client {
	clone := *srv.Client()
	clone.Timeout = d
	return &clone
}

func TestAPIClientState(t *testing.T) {
	seen := map[string]bool{}
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/state" {
			http.Error(w, "bad path", 404)
			return
		}
		seen["auth"] = r.Header.Get("Authorization") == "Bearer tok"
		seen["device"] = r.URL.Query().Get("device") == "s-123"
		fmt.Fprint(w, `{"articles":[{"bookmark_id":"b1","title":"T","author":"A","url":"http://x/kepub/b1","etag":"e1","action":"add","updated":"2026-09-07T10:00:00Z","created":"2026-09-01T08:00:00Z"}],"next_cursor":null}`)
	})
	client := &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, 5*time.Second)}
	articles, err := client.fetchState(context.Background(), "s-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(articles) != 1 || articles[0].BookmarkID != "b1" || articles[0].Action != "add" {
		t.Fatalf("articles = %+v", articles)
	}
	if articles[0].Created != "2026-09-01T08:00:00Z" {
		t.Errorf("created = %q, want date-added", articles[0].Created)
	}
	if articles[0].Updated != "2026-09-07T10:00:00Z" {
		t.Errorf("updated = %q", articles[0].Updated)
	}
	if !seen["auth"] || !seen["device"] {
		t.Errorf("request flags: %v", seen)
	}
}

func TestAPIClientStateErrors(t *testing.T) {
	// Paged response (v1 rejects it).
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"articles":[],"next_cursor":"abc"}`)
	})
	client := &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, time.Second)}
	if _, err := client.fetchState(context.Background(), "s"); err == nil {
		t.Error("paged state should error in v1")
	}

	// 401.
	srv = testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	client = &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, time.Second)}
	_, err := client.fetchState(context.Background(), "s")
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want apiError 401", err)
	}

	// Invalid JSON.
	srv = testServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{not json`)
	})
	client = &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, time.Second)}
	if _, err := client.fetchState(context.Background(), "s"); err == nil {
		t.Error("bad JSON should error")
	}
}

func TestAPIClientKepub(t *testing.T) {
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/kepub/b1" {
			http.Error(w, "bad path", 404)
			return
		}
		fmt.Fprint(w, "PK\x03\x04fake-epub-bytes")
	})
	client := &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, time.Second)}
	data, err := client.fetchKepub(context.Background(), srv.URL+"/api/kepub/b1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PK\x03\x04fake-epub-bytes" {
		t.Fatalf("kepub bytes = %q", data)
	}
	if _, err := client.fetchKepub(context.Background(), "file:///etc/passwd"); err == nil {
		t.Error("non-http URL should be rejected")
	}
	srv2 := testServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", 404) })
	if _, err := client.fetchKepub(context.Background(), srv2.URL+"/x"); err == nil {
		t.Error("404 should error")
	}
}

func TestAPIClientAnnotations(t *testing.T) {
	var got annotationRequest
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "no content type", 400)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fmt.Fprint(w, `{"results":[
			{"bookmark_row_id":"r1","status":"created","annotation_id":"a1","error":""},
			{"bookmark_row_id":"r2","status":"error","error":"boom"}
		]}`)
	})
	client := &apiClient{base: srv.URL, token: "tok", httpc: testHTTPClient(srv, time.Second)}
	items := []uploadItem{{
		BookmarkID: "b1", BookmarkRowID: "r1", StartPath: `span#kobo\.1\.1`, StartOffset: 0,
		EndPath: `span#kobo\.1\.1`, EndOffset: 12, Text: "hello", Annotation: "", Type: "highlight",
		DateCreated: "2026-01-01T00:00:00Z", DateModified: "2026-01-01T00:00:00Z",
	}, {BookmarkRowID: "r2"}}
	results, err := client.postAnnotations(context.Background(), "dev-1", items)
	if err != nil {
		t.Fatal(err)
	}
	if got.Device != "dev-1" || len(got.Items) != 2 {
		t.Fatalf("request = %+v", got)
	}
	if got.Items[0].StartOffset != 0 || got.Items[0].EndOffset != 12 {
		t.Errorf("offsets not integers: %+v", got.Items[0])
	}
	if r := results["r1"]; r.Status != "created" || r.AnnotationID != "a1" {
		t.Errorf("r1 = %+v", r)
	}
	if r := results["r2"]; r.Status != "error" || r.Error != "boom" {
		t.Errorf("r2 = %+v", r)
	}
	if len(results) != 2 {
		t.Errorf("results len = %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// local state (index + ledger)

func TestIndexStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")

	idx := newIndexStore(path)
	idx.set(indexEntry{Filename: "a.kepub.epub", BookmarkID: "b1", ETag: "e1", State: stateDownloaded})
	idx.set(indexEntry{Filename: "b.kepub.epub", BookmarkID: "b2", ETag: "e2", State: stateImported})
	idx.set(indexEntry{Filename: "c.kepub.epub", BookmarkID: "b3", ETag: "e3", State: stateRemoved})
	if err := idx.save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.entries()) != 3 {
		t.Fatalf("entries = %d", len(loaded.entries()))
	}
	names := loaded.entries()
	if names[0].Filename != "a.kepub.epub" || names[2].Filename != "c.kepub.epub" {
		t.Errorf("entries not sorted: %v", names)
	}
	if len(loaded.managed()) != 2 {
		t.Errorf("managed = %d", len(loaded.managed()))
	}
	if len(loaded.pending()) != 1 || loaded.pending()[0].Filename != "a.kepub.epub" {
		t.Errorf("pending = %+v", loaded.pending())
	}
}

func TestIndexStoreCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	os.WriteFile(path, []byte("{not json"), 0o644)

	idx, err := loadIndex(path)
	if err == nil {
		t.Fatal("corrupt index should report an error (and start empty)")
	}
	if len(idx.entries()) != 0 {
		t.Errorf("expected empty store")
	}
	// The corrupt file is preserved for forensics.
	matches, _ := filepath.Glob(filepath.Join(dir, "index.json.corrupt-*"))
	if len(matches) == 0 {
		t.Error("corrupt index not preserved")
	}
}

func TestLedgerRoundtripAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	led := newLedgerStore(path)
	led.set(ledgerEntry{BookmarkRowID: "r1", Status: "created", AnnotationID: "a1", DeviceDateModified: "2026-01-01T00:00:00Z"})
	if err := led.save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := loaded.get("r1")
	if !ok || e.Status != "created" || e.AnnotationID != "a1" || e.DeviceDateModified != "2026-01-01T00:00:00Z" {
		t.Fatalf("ledger entry = %+v (ok=%v)", e, ok)
	}

	os.WriteFile(path, []byte("garbage"), 0o644)
	led2, err := loadLedger(path)
	if err == nil {
		t.Fatal("corrupt ledger should error")
	}
	if led2.count() != 0 {
		t.Errorf("corrupt ledger not empty")
	}
}

// ---------------------------------------------------------------------------
// logging

func TestLoggerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	logger, err := newLogger(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	// Write enough to cross the 1 MiB threshold repeatedly.
	big := strings.Repeat("x", 300*1024)
	for i := 0; i < 8; i++ {
		logger.Info("%s", big)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxLogBytes {
		t.Fatalf("log size %d exceeds %d (rotation failed)", st.Size(), maxLogBytes)
	}

	// newLogger truncates an oversized file left by a previous session.
	bigFile := filepath.Join(dir, "big.log")
	os.WriteFile(bigFile, []byte(strings.Repeat("x", 2<<20)), 0o644)
	l2, err := newLogger(bigFile, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l2.Info("small")
	l2.Close()
	st, _ = os.Stat(bigFile)
	if st.Size() > maxLogBytes {
		t.Fatalf("oversized startup file not truncated: %d", st.Size())
	}
}

// ---------------------------------------------------------------------------
// parseFlags

func TestParseFlags(t *testing.T) {
	opts, err := parseFlags([]string{"--once", "--interval", "5m"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.once || opts.interval != 5*time.Minute {
		t.Errorf("opts = %+v", opts)
	}
	if opts.configPath != defaultConfigPath || opts.onboard != defaultOnboard || opts.koboDB != defaultKoboDBPath {
		t.Errorf("defaults wrong: %+v", opts)
	}
	if _, err := parseFlags([]string{"extra"}, io.Discard); err == nil {
		t.Error("positional args should error")
	}
	if _, err := parseFlags([]string{"--interval", "0s"}, io.Discard); err == nil {
		t.Error("zero interval should error")
	}
}
