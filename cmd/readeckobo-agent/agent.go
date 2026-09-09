package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Agent is one sync pass orchestration unit. Steps per run (see docs/AGENT.md
// for the rationale of each):
//
//  1. GET /api/agent/state (server-side article list for this device).
//  2. Reconcile files: download add/update articles into
//     <onboard>/.kobo/readeck/<sanitized-title>-<bookmark_id>.kepub.epub
//     (skip when our index already has the same etag), delete removed ones.
//  3. When files changed (or an earlier import is still pending): run the
//     NickelDBus rescan (qndb) unless --no-rescan, then poll the content
//     table until every pending ContentID appears (timeout/interval tunable).
//  4. Maintenance on the device DB: ensure content.___SyncTime +
//     content.DateCreated = article.created (Readeck date added) for every
//     managed book row, ensure the collection shelf exists and owns every
//     imported book row with ShelfContent.DateModified = article.created.
//     Never inserts content rows. The ensure is idempotent (created is
//     immutable) so devices synced before the date-added fix self-heal.
//  5. Highlight scan: snapshot KoboReader.sqlite (WAL-safe), extract
//     highlights/notes whose VolumeID is one of our files, diff against the
//     ledger, POST new/changed ones, record the outcomes.
//
// Every step failure is logged and isolated — one broken step must not stop
// the rest of the pass (a server hiccup should not block highlight upload).
type Agent struct {
	cfg     *Config
	log     *Logger
	client  *apiClient
	httpc   *http.Client // http.Client is intentionally shared/test-injectable
	onboard string       // onboard storage root
	koboDB  string       // KoboReader.sqlite path
	dataDir string       // sidecar directory

	idx *indexStore
	led *ledgerStore

	noRescan bool
	snapshot string // snapshot copy path for the WAL-safe read
	pollWait time.Duration
	pollTime time.Duration

	// now is injectable for deterministic timestamps in tests.
	now func() time.Time

	// rescan is the NickelDBus invocation; injectable for tests.
	rescan func(ctx context.Context) error
}

type agentOptions struct {
	cfg      *Config
	logger   *Logger
	onboard  string
	koboDB   string
	dataDir  string
	noRescan bool
	snapshot string
	pollWait time.Duration
	pollTime time.Duration
	httpc    *http.Client
}

func newAgent(opts agentOptions) (*Agent, error) {
	cfg := opts.cfg
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	logger := opts.logger
	if logger == nil {
		var err error
		logger, err = newLogger("", nil)
		if err != nil {
			return nil, err
		}
	}
	onboard := opts.onboard
	if onboard == "" {
		onboard = defaultOnboard
	}
	koboDB := opts.koboDB
	if koboDB == "" {
		koboDB = filepath.Join(onboard, ".kobo", "KoboReader.sqlite")
	}
	snapshot := opts.snapshot
	if snapshot == "" {
		snapshot = "/tmp/readeckobo-agent.sqlite"
	}
	pollWait := opts.pollWait
	if pollWait <= 0 {
		pollWait = 2 * time.Second
	}
	pollTime := opts.pollTime
	if pollTime <= 0 {
		pollTime = 180 * time.Second
	}
	var httpc *http.Client
	if opts.httpc != nil {
		httpc = opts.httpc // test-injectable
	} else {
		var err error
		httpc, err = defaultHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}

	idx, err := loadIndex(filepath.Join(opts.dataDir, "index.json"))
	if err != nil {
		logger.Warn("%v", err)
	}
	led, err := loadLedger(filepath.Join(opts.dataDir, "ledger.json"))
	if err != nil {
		logger.Warn("%v", err)
	}

	a := &Agent{
		cfg:      cfg,
		log:      logger,
		onboard:  onboard,
		koboDB:   koboDB,
		dataDir:  opts.dataDir,
		idx:      idx,
		led:      led,
		noRescan: opts.noRescan,
		snapshot: snapshot,
		pollWait: pollWait,
		pollTime: pollTime,
		now:      time.Now,
		httpc:    httpc,
	}
	client := newAPIClient(cfg)
	client.httpc = httpc
	if cfg.HTTPTimeout > 0 {
		// http.Client.Timeout covers headers + response body reads and
		// only cancels when the deadline actually expires (the per-request
		// WithTimeout in do() previously canceled the context as soon as
		// the request returned, killing the body decode).
		httpc.Timeout = cfg.HTTPTimeout
	}
	a.client = client
	a.rescan = func(ctx context.Context) error {
		return runQNDBRescan(ctx, cfg.QNDBPath, cfg.QNDBArgs, logger)
	}
	return a, nil
}

// defaultHTTPClient builds the shared HTTP client used when the caller did
// not inject one (tests and the CLI). TLS trust:
//
//   - Public-CA servers: the embedded Mozilla bundle (certs.go) is registered
//     with x509.SetFallbackRoots(), so HTTPS verification works on the Kobo,
//     where no system CA store exists. Fallback roots are used only when the
//     system pool is unavailable, never instead of it.
//   - CA_FILE (private CA): the configured PEM bundle is installed as the
//     transport's TLS RootCAs, so the client trusts exactly that bundle.
//
// InsecureSkipVerify is deliberately never used.
func defaultHTTPClient(cfg *Config) (*http.Client, error) {
	if cfg.CAFile == "" {
		return &http.Client{}, nil
	}
	pool, err := caPoolFromFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("agent: CA_FILE %s: %w", cfg.CAFile, err)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}, nil
}

// kepubDir is the on-board store directory for kepubs.
func (a *Agent) kepubDir() string {
	return filepath.Join(a.onboard, ".kobo", "readeck")
}

// syncOnce runs a full pass. A returned error means the pass could not even
// start meaningfully (server unreachable, DB missing); step-level failures
// are logged inside and the pass continues.
func (a *Agent) syncOnce(ctx context.Context) error {
	if err := a.ensureDirs(); err != nil {
		return err
	}

	articles, err := a.client.fetchState(ctx, a.cfg.DeviceSerial)
	if err != nil {
		return fmt.Errorf("step 1 (state): %w", err)
	}
	if len(articles) == 0 {
		a.log.Info("server reported no articles for device %s", a.cfg.DeviceSerial)
	}
	// Note: article duplicates (same bookmark_id twice) — last one wins.
	seen := map[string]Article{}
	for _, art := range articles {
		seen[art.BookmarkID] = art
	}
	articles = make([]Article, 0, len(seen))
	for _, art := range seen {
		articles = append(articles, art)
	}
	// Deterministic oldest-first order (by date-added, then id): bulk imports
	// create files in Readeck order so even file mtimes ascend, and logs read
	// chronologically. The DB sort itself uses explicit timestamps, so this is
	// belt-and-braces, not the sort mechanism.
	sort.Slice(articles, func(i, j int) bool {
		ci, cj := articles[i].Created, articles[j].Created
		if ci == "" {
			ci = articles[i].Updated
		}
		if cj == "" {
			cj = articles[j].Updated
		}
		if ci != cj {
			return ci < cj
		}
		return articles[i].BookmarkID < articles[j].BookmarkID
	})

	// --- step 2: files ---
	changed, err := a.reconcileFiles(ctx, articles)
	if err != nil {
		// Continue with what we have; downloads that failed stay pending.
		a.log.Warn("file reconcile had errors: %v", err)
	}

	// --- step 3: rescan + poll for imports ---
	pendingBefore := len(a.idx.pending())
	if !a.noRescan && (changed || pendingBefore > 0) {
		if err := a.rescanAndPoll(ctx); err != nil {
			a.log.Warn("import poll incomplete: %v", err)
		}
	} else if a.noRescan && pendingBefore > 0 {
		a.log.Info("--no-rescan: skipping NickelDBus rescan for %d pending import(s)", pendingBefore)
	}

	// --- step 4: date-added + collection maintenance ---
	if err := a.maintainCollection(ctx, articles); err != nil {
		a.log.Warn("collection maintenance failed: %v", err)
	}

	// --- step 5: highlights ---
	if err := a.syncHighlights(ctx); err != nil {
		a.log.Warn("highlight sync failed: %v", err)
	}

	a.summarize()
	return nil
}

func (a *Agent) summarize() {
	pending := len(a.idx.pending())
	logged := len(a.idx.entries())
	a.log.Info("pass complete: %d managed file(s), %d pending import(s), %d ledger row(s)",
		logged, pending, a.led.count())
}

func (a *Agent) ensureDirs() error {
	for _, dir := range []string{a.dataDir, a.kepubDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// step 2 — file reconcile

// reconcileFiles brings the kepub directory in line with the server state:
// downloads add/update articles whose etag we have not seen, deletes removed
// ones. Returns whether anything changed on disk.
func (a *Agent) reconcileFiles(ctx context.Context, articles []Article) (bool, error) {
	changed := false
	var firstErr error
	report := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, art := range articles {
		filename := kepubFilename(art.Title, art.BookmarkID)
		switch art.Action {
		case "add", "update":
			ok, err := a.ensureKepub(ctx, art, filename)
			if err != nil {
				a.log.Error("download %s failed: %v", filename, err)
				report(err)
				continue
			}
			changed = changed || ok
		case "remove":
			if err := a.removeKepub(filename, art.BookmarkID); err != nil {
				a.log.Error("remove %s failed: %v", filename, err)
				report(err)
				continue
			}
			changed = true
		default:
			a.log.Warn("state article %s has unknown action %q; ignoring", art.BookmarkID, art.Action)
		}
	}
	// Sweep managed files the feed no longer mentions. The server emits
	// each remove exactly once and then drops its ledger row, so a device
	// that missed that emission (offline, or the remove was consumed by a
	// different client sharing the device id) would otherwise keep the
	// file — and its shelf entry — forever. The feed carries every live
	// bookmark on every call, so anything we still manage but the feed no
	// longer lists is gone server-side (archived, deleted, or excluded
	// like video bookmarks): treat it as removed.
	feedIDs := make(map[string]bool, len(articles))
	for _, art := range articles {
		feedIDs[art.BookmarkID] = true
	}
	for _, e := range a.idx.managed() {
		if feedIDs[e.BookmarkID] {
			continue
		}
		a.log.Info("sweep: %s (bookmark %s) no longer in state feed; removing", e.Filename, e.BookmarkID)
		if err := a.removeKepub(e.Filename, e.BookmarkID); err != nil {
			a.log.Error("sweep remove %s failed: %v", e.Filename, err)
			report(err)
			continue
		}
		changed = true
	}
	if err := a.idx.save(); err != nil {
		a.log.Warn("index save failed: %v", err)
	}
	return changed, firstErr
}

// ensureKepub downloads the article when the current file does not match the
// server etag. Returns changed=true when the file was (re)written.
//
// Lifecycle note: re-downloading an *already imported* book (server sent an
// update with a new etag) keeps the entry imported — Nickel re-uses the
// existing content row on rescan, so the date-added columns converge
// idempotently to article.created every pass (created is immutable, unlike
// updated, so re-ensuring is harmless).
func (a *Agent) ensureKepub(ctx context.Context, art Article, filename string) (bool, error) {
	created := art.Created
	if created == "" {
		// Old servers predate the created field: fall back to updated so the
		// device still gets a usable (if less stable) timestamp.
		created = art.Updated
	}
	prev, hadPrev := a.idx.get(filename)
	if hadPrev && prev.ETag == art.ETag && prev.State != stateRemoved {
		path := filepath.Join(a.kepubDir(), filename)
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			// Up-to-date: backfill the index when the feed carries newer
			// metadata (created backfill for devices synced before the
			// date-added fix, title updates, ...). No download, no rescan.
			if prev.Created != created || prev.Updated != art.Updated || prev.Title != art.Title || prev.BookmarkID != art.BookmarkID {
				prev.Created = created
				prev.Updated = art.Updated
				prev.Title = art.Title
				if art.BookmarkID != "" {
					prev.BookmarkID = art.BookmarkID
				}
				prev.LastError = ""
				a.idx.set(prev)
			}
			a.log.Info("up-to-date: %s (etag %s)", filename, art.ETag)
			return false, nil
		}
		// File vanished but the index says it was there: re-download below.
		a.log.Warn("%s is in the index but missing on disk; re-downloading", filename)
	}
	if art.URL == "" {
		return false, fmt.Errorf("article %s has no kepub url", art.BookmarkID)
	}
	a.log.Info("downloading %s (etag %s)…", filename, art.ETag)

	data, err := a.client.fetchKepub(ctx, art.URL)
	if err != nil {
		return false, err
	}
	// Download to a temp file in the same directory, then rename into place:
	// an interrupted download must never look like a valid kepub.
	tmp, err := os.CreateTemp(a.kepubDir(), ".download-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	final := filepath.Join(a.kepubDir(), filename)
	if err := os.Rename(tmpName, final); err != nil {
		return false, err
	}
	state := stateDownloaded
	if hadPrev && prev.State == stateImported {
		state = stateImported
	}
	a.idx.set(indexEntry{
		Filename:   filename,
		BookmarkID: art.BookmarkID,
		Title:      art.Title,
		ETag:       art.ETag,
		Updated:    art.Updated,
		Created:    created,
		State:      state,
	})
	return true, nil
}

// removeKepub deletes the kepub file and tombstones the index entry. The
// content rows are deliberately left alone in v1: Nickel removes stale rows
// on the next rescan (or the user deletes them manually) — documented in
// docs/AGENT.md.
func (a *Agent) removeKepub(filename, bookmarkID string) error {
	e, ok := a.idx.get(filename)
	if !ok {
		a.log.Info("remove for unknown file %s; nothing to do", filename)
		return nil
	}
	path := filepath.Join(a.kepubDir(), filename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.State = stateRemoved
	e.RemovedAt = formatKoboTime(a.now())
	if bookmarkID != "" {
		e.BookmarkID = bookmarkID
	}
	a.idx.set(e)
	a.log.Info("removed %s (tombstoned)", filename)
	return nil
}

// ---------------------------------------------------------------------------
// step 3 — NickelDBus rescan and import polling

// rescanAndPoll runs the NickelDBus rescan and then polls the content table
// until every pending kepub has a book row (Nickel imported it). Polling
// reads the live DB read-only; SQLite gives us a consistent snapshot per
// statement and WAL readers never block the Nickel writer.
func (a *Agent) rescanAndPoll(ctx context.Context) error {
	if err := a.rescan(ctx); err != nil {
		if errors.Is(err, errQNDBNotFound) {
			// No rescan tool at all: rows cannot appear; do not burn the
			// poll window. Pending entries retry next pass.
			return err
		}
		// Even when qndb reports failure the import may still be in flight;
		// poll anyway — it self-heals on later runs too (pending entries
		// trigger a new rescan attempt every pass).
		a.log.Warn("rescan command failed: %v", err)
	}
	return a.pollForImports(ctx)
}

// pollForImports waits until every pending import shows up in the content
// table, up to a.pollTime at a.pollWait intervals.
func (a *Agent) pollForImports(ctx context.Context) error {
	deadline := time.Now().Add(a.pollTime)
	for {
		remaining, err := a.pendingMissing(ctx)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			a.log.Info("all %d pending import(s) present in the content table", len(a.idx.pending()))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s: still missing content rows for %s", a.pollTime, strings.Join(remaining, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.pollWait):
		}
	}
}

// pendingMissing returns the names of pending kepubs that have no book row in
// the content table yet.
func (a *Agent) pendingMissing(ctx context.Context) ([]string, error) {
	db, err := openReadOnly(a.koboDB)
	if err != nil {
		return nil, fmt.Errorf("opening %s read-only: %w", a.koboDB, err)
	}
	defer db.Close()
	rows, err := readManagedBookRows(ctx, db)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, e := range a.idx.pending() {
		if _, ok := rows[e.Filename]; !ok {
			missing = append(missing, e.Filename)
		}
	}
	return missing, nil
}

// errQNDBNotFound marks a rescan that cannot run at all (binary missing).
var errQNDBNotFound = errors.New("qndb rescan unavailable")

// runQNDBRescan shells out to the NickelDBus CLI:
//
//	qndb -t 30000 -s pfmDoneProcessing -m pfmRescanBooksFull
//
// (interface com.github.shermp.nickeldbus; see docs/research/kobo-db-schema.md
// §7). A nonzero exit is treated as a warning — NickelDBus times out while
// the import continues in the background — but a missing binary is an error.
func runQNDBRescan(ctx context.Context, qndbPath string, args []string, log *Logger) error {
	log.Info("rescan: %s %s", qndbPath, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, qndbPath, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg != "" {
		log.Warn("qndb output: %s", msg)
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("qndb not found at %s (install NickelDBus or fix QNDB_PATH): %w", qndbPath, errQNDBNotFound)
	}
	// Nonzero exit (commonly the signal timeout) — the import keeps running.
	log.Warn("qndb exited with %v (the import may still be in progress)", err)
	return nil
}

// ---------------------------------------------------------------------------
// step 4 — date-added + collection maintenance

// desiredCreated resolves the authoritative "date added" for a managed entry:
// the feed's created when present (covers backfill for indexes written before
// the created field existed), else the index's stored created, else the legacy
// updated fallback for old servers.
func desiredCreated(e indexEntry, feed map[string]Article) string {
	if art, ok := feed[e.BookmarkID]; ok && art.Created != "" {
		return art.Created
	}
	if e.Created != "" {
		return e.Created
	}
	return e.Updated
}

// maintainCollection ensures the Kobo sort keys converge to the Readeck date
// added (bookmark.created) and maintains the Shelf/ShelfContent membership.
// It only touches book rows whose ContentID is one of our kepubs. Unlike the
// original one-shot DateCreated stamping, the ensure is idempotent: created
// is immutable, so every pass converges already-imported rows (self-healing
// devices synced before the fix, and rows Nickel re-created on etag updates)
// instead of leaving them at import time. The shelf/membership ensure is
// idempotent and self-healing (it also repairs a shelf the user deleted or a
// membership row lost to a crash).
func (a *Agent) maintainCollection(ctx context.Context, articles []Article) error {
	managed := a.idx.managed()
	if len(managed) == 0 {
		return nil
	}

	feed := make(map[string]Article, len(articles))
	for _, art := range articles {
		// Last one wins on duplicates, matching syncOnce dedup.
		feed[art.BookmarkID] = art
	}

	ro, err := openReadOnly(a.koboDB)
	if err != nil {
		return fmt.Errorf("opening %s read-only: %w", a.koboDB, err)
	}
	present, err := readManagedBookRows(ctx, ro)
	_ = ro.Close()
	if err != nil {
		return err
	}
	if len(present) == 0 {
		a.log.Info("no imported book rows yet; date-added/collection step skipped")
		return nil
	}

	rw, err := openReadWrite(a.koboDB)
	if err != nil {
		return fmt.Errorf("opening %s read-write: %w", a.koboDB, err)
	}
	defer rw.Close()

	version, err := dbVersion(ctx, rw)
	if err != nil {
		return fmt.Errorf("cannot determine DbVersion; skipping DB writes: %w", err)
	}

	// 4a. content.___SyncTime + content.DateCreated = article.created for every
	// managed book row (idempotent ensure, not once-only).
	stamped := 0
	for _, e := range managed {
		row, ok := present[e.Filename]
		if !ok {
			continue // not imported yet; leave pending for a later pass
		}
		raw := desiredCreated(e, feed)
		ts, err := parseKoboTime(raw)
		if err != nil {
			if e.State == stateDownloaded {
				e.LastError = fmt.Sprintf("unparseable article.created %q: %v", raw, err)
				a.idx.set(e)
				a.log.Warn("keeping %s pending: %s", e.Filename, e.LastError)
			} else {
				a.log.Warn("skipping date ensure for %s: unparseable article.created %q: %v", e.Filename, raw, err)
			}
			continue
		}
		want := formatKoboTime(ts)
		// Backfill the index so the next pass does not depend on the feed.
		if e.Created == "" || (feed[e.BookmarkID].Created != "" && e.Created != feed[e.BookmarkID].Created) {
			e.Created = raw
			e.LastError = ""
			a.idx.set(e)
		}
		if row.DateCreated != want || row.SyncTime != want {
			if err := setBookAddedDate(ctx, rw, row.ContentID, want); err != nil {
				a.log.Warn("date-added update failed for %s: %v", e.Filename, err)
				continue
			}
			stamped++
		}
		if e.State == stateDownloaded {
			e.State = stateImported
			e.LastError = ""
			a.idx.set(e)
		}
	}
	if stamped > 0 {
		a.log.Info("ensured date-added (___SyncTime/DateCreated) on %d book row(s)", stamped)
	}

	// 4b. Collection membership for every managed kepub that has a book row,
	// with ShelfContent.DateModified = article.created (not the sync time).
	if err := ensureShelf(ctx, rw, a.cfg.Collection, a.now(), version); err != nil {
		return fmt.Errorf("ensure shelf %q: %w", a.cfg.Collection, err)
	}
	for _, e := range managed {
		row, ok := present[e.Filename]
		if !ok {
			continue
		}
		raw := desiredCreated(e, feed)
		ts, err := parseKoboTime(raw)
		if err != nil {
			a.log.Warn("shelf membership for %s skipped: unparseable article.created %q", e.Filename, raw)
			continue
		}
		if err := addContentToShelf(ctx, rw, a.cfg.Collection, row.ContentID, formatKoboTime(ts)); err != nil {
			a.log.Warn("shelf membership for %s failed: %v", e.Filename, err)
		}
	}
	a.log.Info("collection %q (DbVersion %d) has %d book row(s)", a.cfg.Collection, version, len(present))
	return a.idx.save()
}

// ---------------------------------------------------------------------------
// step 5 — highlights

// syncHighlights snapshots the DB (WAL-safe), extracts highlights of our
// books, diffs them against the ledger and uploads the new/changed ones.
func (a *Agent) syncHighlights(ctx context.Context) error {
	managed := a.idx.managed()
	if len(managed) == 0 {
		a.log.Info("no managed books; highlight scan skipped")
		return nil
	}
	names := map[string]indexEntry{}
	for _, e := range managed {
		names[e.Filename] = e
	}

	if err := snapshotKoboDB(ctx, a.koboDB, a.snapshot); err != nil {
		return err
	}
	db, err := openReadOnly(a.snapshot)
	if err != nil {
		return err
	}
	defer db.Close()

	// Only books that actually have a content row are readable on the
	// device; rows for deleted files are stale and must not be uploaded.
	rows, err := readManagedBookRows(ctx, db)
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for name := range rows {
		if _, ok := names[name]; ok {
			present[name] = true
		}
	}
	if len(present) == 0 {
		a.log.Info("none of the managed kepubs has a content row yet; highlight scan skipped")
		return nil
	}

	highlights, err := scanHighlights(ctx, db)
	if err != nil {
		return err
	}
	a.log.Info("highlight scan: %d candidate row(s) in device DB", len(highlights))

	var items []uploadItem
	skipped := 0
	for _, h := range highlights {
		name, ok := managedFilename(h.VolumeID, a.idx)
		if !ok || !present[name] {
			continue // row for a kepub that is removed or not (yet) imported
		}
		entry := names[name]
		eff := effectiveModified(h)
		if eff == "" {
			a.log.Warn("bookmark row %s has neither DateModified nor DateCreated; skipping (cannot change-track)", h.RowID)
			a.recordSkipped(h, entry, "device row has no DateModified/DateCreated")
			skipped++
			continue
		}
		if le, ok := a.led.get(h.RowID); ok && le.DeviceDateModified == eff {
			continue // already sent in this exact state
		}
		item, ok := buildUploadItem(h, entry.BookmarkID, eff)
		if !ok {
			a.log.Warn("bookmark row %s has unparseable offsets (start=%q end=%q); skipping", h.RowID,
				nullString(h.StartOffset), nullString(h.EndOffset))
			a.recordSkipped(h, entry, "unparseable offsets")
			skipped++
			continue
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		a.log.Info("no new highlights to upload (%d scanned, %d skipped this pass)", len(highlights), skipped)
		return a.led.save()
	}
	a.log.Info("uploading %d highlight(s)/note(s) to device %s", len(items), a.cfg.DeviceSerial)
	results, err := a.client.postAnnotations(ctx, a.cfg.DeviceSerial, items)
	if err != nil {
		// Nothing recorded → the batch is retried next pass.
		return err
	}

	recorded := 0
	rowOf := map[string]deviceHighlight{}
	for _, h := range highlights {
		rowOf[h.RowID] = h
	}
	for _, item := range items {
		res, ok := results[item.BookmarkRowID]
		if !ok {
			a.log.Warn("server response has no result for bookmark row %s; leaving for next pass", item.BookmarkRowID)
			continue
		}
		volumeID := ""
		if h, ok := rowOf[item.BookmarkRowID]; ok {
			volumeID = h.VolumeID
		}
		switch res.Status {
		case "created", "updated", "unchanged", "skipped":
			a.led.set(ledgerEntry{
				BookmarkRowID:      item.BookmarkRowID,
				BookmarkID:         item.BookmarkID,
				VolumeID:           volumeID,
				DeviceDateModified: item.DateModified,
				Status:             res.Status,
				AnnotationID:       res.AnnotationID,
				LastError:          res.Error,
				SentAt:             formatKoboTime(a.now()),
			})
			recorded++
			if res.Status == "created" || res.Status == "updated" {
				a.log.Info("annotation %s (%s): %s", item.BookmarkRowID, item.Type, res.Status)
			}
		default:
			// Error/unknown status: do not record — retry next pass.
			a.log.Warn("annotation %s rejected: status=%q error=%q; will retry", item.BookmarkRowID, res.Status, res.Error)
		}
	}
	if err := a.led.save(); err != nil {
		a.log.Warn("ledger save failed: %v", err)
	}
	a.log.Info("ledger: %d recorded, %d left for next pass", recorded, len(items)-recorded)
	return nil
}

// recordSkipped writes a definitive "skipped" ledger entry (with the reason)
// so a device row that can never be uploaded is not retried every pass.
func (a *Agent) recordSkipped(h deviceHighlight, entry indexEntry, reason string) {
	a.led.set(ledgerEntry{
		BookmarkRowID:      h.RowID,
		BookmarkID:         entry.BookmarkID,
		VolumeID:           h.VolumeID,
		DeviceDateModified: effectiveModified(h),
		Status:             "skipped",
		LastError:          reason,
		SentAt:             formatKoboTime(a.now()),
	})
}
