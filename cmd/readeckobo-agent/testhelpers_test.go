package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixture database generation (tools/kobofixdb, its own nested Go module).
//
// The generator is built once per test binary into a scratch dir and then
// run with OUT=<scratch>; tests copy KoboReader.sqlite out of there when
// they need to mutate it. The fixture is documented row-by-row in
// docs/research/kobo-fixture-db.md.

var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixtureErr  error
)

// TestMain runs the package's tests and then removes the shared fixture dir
// created lazily by ensureFixture. That dir is generated once per test binary
// (sync.Once) and reused by every test that copies KoboReader.sqlite out of
// it, so its lifetime spans the whole run and can't be tied to a single test's
// t.TempDir() cleanup. Removing it here keeps /tmp free of
// readeckobo-fixture-* litter after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func ensureFixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "readeckobo-fixture-*")
		if err != nil {
			fixtureErr = err
			return
		}
		abs, err := filepath.Abs("../../tools/kobofixdb")
		if err != nil {
			fixtureErr = err
			return
		}
		bin := filepath.Join(dir, "kobofixdb")
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Dir = abs
		if out, err := build.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("building tools/kobofixdb: %v\n%s", err, out)
			return
		}
		run := exec.Command(bin)
		run.Env = append(os.Environ(), "OUT="+dir)
		if out, err := run.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("running kobofixdb: %v\n%s", err, out)
			return
		}
		fixtureDir = dir
	})
	if fixtureErr != nil {
		t.Fatalf("fixture generation failed: %v", fixtureErr)
	}
	return fixtureDir
}

// copyFixtureDB copies the pristine fixture into dstDir/KoboReader.sqlite and
// returns the path.
func copyFixtureDB(t *testing.T, dstDir string) string {
	t.Helper()
	src := filepath.Join(ensureFixture(t), "KoboReader.sqlite")
	dst := filepath.Join(dstDir, "KoboReader.sqlite")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}
	return dst
}

// openFixtureRW opens a DB path for direct (test-side) manipulation.
func openFixtureRW(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openReadWrite(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// execDB runs one statement for test setup.
func execDB(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// scanOne reads one cell for assertions.
func scanOne(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s.String
}

// ---------------------------------------------------------------------------
// Whole-loop test environment: an httptest server (readeckobo stand-in), an
// onboard tree with a rewritable fixture DB, and the agent data dir.

const (
	bookAID    = "bmA1"
	bookBID    = "bmB1"
	testToken  = "test-token-123"
	testSerial = "device-serial-abc"
)

// fixturePathRewrites moves the two fixture books from their original paths
// to the agent's managed directory with agent-style file names:
//
//	.kobo/readeck/Readeck Article One.kepub.epub   → ...Readeck Article One-bmA1.kepub.epub
//	books/Project Hail Mary - Andy Weir.kepub.epub → .kobo/readeck/Project Hail Mary-bmB1.kepub.epub
//
// (Book B starts in the normal library path; the agent only manages books it
// imports into .kobo/readeck, so the loop test moves it there.)
var fixturePathRewrites = [][2]string{
	{".kobo/readeck/Readeck Article One.kepub.epub", ".kobo/readeck/Readeck Article One-bmA1.kepub.epub"},
	{"books/Project Hail Mary - Andy Weir.kepub.epub", ".kobo/readeck/Project Hail Mary-bmB1.kepub.epub"},
}

// setupLoopEnv creates:
//
//   - onboard/ (the --onboard root) with .kobo/KoboReader.sqlite — a fixture
//     copy whose file:///mnt/onboard/... VolumeIDs/ContentIDs are rewritten
//     to the real temp onboard path and whose fixture file names are renamed
//     to the agent's "<title>-<bookmark_id>" convention;
//   - dataDir/ (the --config directory) for index.json/ledger.json;
//   - a scripted readeckobo server (state + kepub + annotations endpoints).
//
// Articles are served from *currentState, which the test swaps between runs.
type loopEnv struct {
	t        *testing.T
	server   *httptest.Server
	onboard  string
	dataDir  string
	koboDB   string
	snapshot string

	mu           sync.Mutex
	currentState []Article
	kepubBodies  map[string]string
	stateHits    int
	postBodies   []annotationRequest
	authHeaders  []string
	resultFn     func(rowID string) annotationResult // optional per-row scripting
}

func newLoopEnv(t *testing.T) *loopEnv {
	t.Helper()
	root := t.TempDir()
	env := &loopEnv{
		t:           t,
		onboard:     filepath.Join(root, "onboard"),
		dataDir:     filepath.Join(root, "data"),
		kepubBodies: map[string]string{},
	}
	env.snapshot = filepath.Join(root, "snapshot.sqlite")

	// Onboard tree with the rewritten fixture DB.
	onboardKobo := filepath.Join(env.onboard, ".kobo")
	env.koboDB = copyFixtureDB(t, onboardKobo)
	rewriteFixturePaths(t, env.koboDB, env.onboard)

	// Drop the pre-seeded "Readeck" shelf + memberships so the tests exercise
	// the agent's create/ensure path end to end.
	db := openFixtureRW(t, env.koboDB)
	execDB(t, db, "DELETE FROM ShelfContent")
	execDB(t, db, "DELETE FROM Shelf")

	env.server = httptest.NewServer(env.handler())
	t.Cleanup(env.server.Close)
	return env
}

// rewriteFixturePaths renames the fixture's books to agent-style names and
// moves VolumeIDs/ContentIDs from file:///mnt/onboard/... to the test's
// onboard root (chapter "!!" suffixes are preserved because only the
// path+book-name portion is replaced).
func rewriteFixturePaths(t *testing.T, dbPath, onboard string) {
	t.Helper()
	db := openFixtureRW(t, dbPath)
	prefix := "file:///mnt/onboard"
	newPrefix := "file://" + filepath.ToSlash(onboard)
	execDB(t, db, `UPDATE content SET ContentID = replace(ContentID, ?, ?) WHERE ContentID LIKE ?`,
		prefix, newPrefix, prefix+"%")
	execDB(t, db, `UPDATE Bookmark SET VolumeID = replace(VolumeID, ?, ?) WHERE VolumeID LIKE ?`,
		prefix, newPrefix, prefix+"%")
	execDB(t, db, `UPDATE Bookmark SET ContentID = replace(ContentID, ?, ?) WHERE ContentID LIKE ?`,
		prefix, newPrefix, prefix+"%")
	execDB(t, db, `UPDATE ShelfContent SET ContentId = replace(ContentId, ?, ?) WHERE ContentId LIKE ?`,
		prefix, newPrefix, prefix+"%")
	for _, rw := range fixturePathRewrites {
		oldToken, newToken := rw[0], rw[1]
		execDB(t, db, `UPDATE content SET ContentID = replace(ContentID, ?, ?) WHERE ContentID LIKE ?`,
			oldToken, newToken, "%"+oldToken+"%")
		execDB(t, db, `UPDATE Bookmark SET VolumeID = replace(VolumeID, ?, ?) WHERE VolumeID LIKE ?`,
			oldToken, newToken, "%"+oldToken+"%")
		execDB(t, db, `UPDATE Bookmark SET ContentID = replace(ContentID, ?, ?) WHERE ContentID LIKE ?`,
			oldToken, newToken, "%"+oldToken+"%")
		execDB(t, db, `UPDATE ShelfContent SET ContentId = replace(ContentId, ?, ?) WHERE ContentId LIKE ?`,
			oldToken, newToken, "%"+oldToken+"%")
	}
}

// setState replaces the article list served by /api/agent/state.
func (e *loopEnv) setState(articles ...Article) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.currentState = articles
}

func (e *loopEnv) state() []Article {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Article, len(e.currentState))
	copy(out, e.currentState)
	return out
}

func (e *loopEnv) setKepub(id, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.kepubBodies[id] = body
}

func (e *loopEnv) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/agent/state":
			e.mu.Lock()
			e.stateHits++
			articles := append([]Article(nil), e.currentState...)
			e.mu.Unlock()
			if got := r.URL.Query().Get("device"); got != testSerial {
				http.Error(w, fmt.Sprintf("unexpected device %q", got), http.StatusBadRequest)
				return
			}
			writeJSON(w, stateResponse{Articles: articles})
		case "/api/kepub/bmA1", "/api/kepub/bmB1", "/api/kepub/bmX":
			e.mu.Lock()
			body := e.kepubBodies[r.URL.Path[len("/api/kepub/"):]]
			e.mu.Unlock()
			if body == "" {
				http.Error(w, "no such kepub", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/epub+zip")
			fmt.Fprint(w, body)
		case "/api/agent/annotations":
			var req annotationRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			e.mu.Lock()
			e.postBodies = append(e.postBodies, req)
			e.authHeaders = append(e.authHeaders, r.Header.Get("Authorization"))
			e.mu.Unlock()
			res := annotationResponse{}
			for _, item := range req.Items {
				r := annotationResult{BookmarkRowID: item.BookmarkRowID, Status: "created"}
				if e.resultFn != nil {
					r = e.resultFn(item.BookmarkRowID)
					r.BookmarkRowID = item.BookmarkRowID
				}
				res.Results = append(res.Results, r)
			}
			writeJSON(w, res)
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// writeJSON is a small helper for the test server.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (e *loopEnv) postCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.postBodies)
}

func (e *loopEnv) lastPost() annotationRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.postBodies[len(e.postBodies)-1]
}

func (e *loopEnv) stateHitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stateHits
}

// writeConfig writes the agent config file.
func (e *loopEnv) writeConfig(overrides map[string]string) {
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	lines := []string{
		"SERVER_URL=" + e.server.URL,
		"TOKEN=" + testToken,
		"DEVICE_SERIAL=" + testSerial,
		"COLLECTION=Readeck",
	}
	for k, v := range overrides {
		lines = append(lines, k+"="+v)
	}
	cfgPath := filepath.Join(e.dataDir, "config")
	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// newAgent builds an agent wired to this env (config loaded from disk so the
// config path is exercised too).
func (e *loopEnv) newAgent(t *testing.T, mut func(*agentOptions)) *Agent {
	t.Helper()
	e.writeConfig(nil)
	cfg, err := loadConfig(filepath.Join(e.dataDir, "config"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	logger, err := newLogger("", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	opts := agentOptions{
		cfg:      cfg,
		logger:   logger,
		onboard:  e.onboard,
		koboDB:   e.koboDB,
		dataDir:  e.dataDir,
		noRescan: true,
		snapshot: e.snapshot,
		pollWait: 5 * time.Millisecond,
		pollTime: 2 * time.Second,
	}
	if mut != nil {
		mut(&opts)
	}
	ag, err := newAgent(opts)
	if err != nil {
		t.Fatalf("newAgent: %v", err)
	}
	return ag
}

// runSync is a convenience wrapper: new agent + one sync pass.
func (e *loopEnv) runSync(t *testing.T) error {
	return e.newAgent(t, nil).syncOnce(context.Background())
}

// volumeID computes the rewritten device id for a managed file name.
func (e *loopEnv) volumeID(filename string) string {
	return volumeIDFor(e.onboard, filename)
}
