package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"readeckobo/internal/kepub"
	"readeckobo/internal/mapper"
	"readeckobo/internal/models"
	"readeckobo/internal/readeck"
	"readeckobo/internal/store"
)

// Agent route handlers: the on-device sync agent's wire contract
// (docs/research/mapping-and-endpoints.md). All three routes authenticate with
// either `Authorization: Bearer <device-token>` or `?token=<device-token>`.

// deviceTokenFromRequest extracts the device token from the Authorization
// header (Bearer) or the ?token= query parameter.
func (a *App) deviceTokenFromRequest(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), true
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t, true
	}
	return "", false
}

// requireAgentUser resolves the device token to a configured user's Readeck
// token. It returns both the raw device token (needed for URL embedding) and
// the Readeck token. On failure it writes a 401 and returns ok=false.
func (a *App) requireAgentUser(w http.ResponseWriter, r *http.Request, route string) (deviceToken, readeckToken string, ok bool) {
	deviceToken, ok = a.deviceTokenFromRequest(r)
	if !ok {
		http.Error(w, "Missing device token", http.StatusUnauthorized)
		a.Logger.Warnf("Missing device token for %s", route)
		return "", "", false
	}
	readeckToken, err := a.getReadeckToken(deviceToken)
	if err != nil {
		http.Error(w, "Invalid device token", http.StatusUnauthorized)
		a.Logger.Warnf("Invalid device token for %s: %v", route, err)
		return "", "", false
	}
	return deviceToken, readeckToken, true
}

// baseURLFromRequest builds the externally visible base URL (scheme://host)
// for building absolute agent URLs (the X-Forwarded-Proto header is honored,
// matching the image-conversion handler).
func baseURLFromRequest(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Header.Get("Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// kepubGenVersion bumps the store cache key when the generator's output
// format changes (e.g. the rd-annotation styling rework): after an upgrade,
// previously cached kepubs are regenerated on first use instead of being
// served stale.
const kepubGenVersion = "v2"

// kepubCacheKey derives the cache-key variant of an article etag. Both kepub
// routes must use it so they share one cache entry per bookmark.
func kepubCacheKey(etag string) string {
	return kepubGenVersion + ":" + etag
}

// etagForBookmark returns the content version marker for a bookmark: the
// bookmark's updated timestamp when available, else a stable hash of its
// metadata. The kepub route falls back to a hash of the article HTML when
// there is no timestamp at all.
func etagForBookmark(bm *readeck.Bookmark) string {
	if t := bookmarkUpdated(bm); t != "" {
		return t
	}
	sum := sha256.Sum256([]byte(bm.URL + "\x00" + bm.Title + "\x00" + bm.Site))
	return hex.EncodeToString(sum[:16])
}

// bookmarkUpdated returns the bookmark's updated timestamp as RFC3339Nano
// (falling back to Created), or "" when the bookmark has no timestamp.
func bookmarkUpdated(bm *readeck.Bookmark) string {
	t := bm.Updated
	if t.IsZero() {
		t = bm.Created
	}
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// kepubMetaFromBookmark builds the kepub metadata from a Readeck bookmark.
func kepubMetaFromBookmark(bm *readeck.Bookmark) kepub.Meta {
	return kepub.Meta{
		Title:     bm.Title,
		Author:    strings.Join(bm.Authors, ", "),
		SourceURL: bm.URL,
	}
}

// ---------------------------------------------------------------------------
// GET /api/agent/state?device=<serial>
// ---------------------------------------------------------------------------

// HandleAgentState serves the agent's state feed: the user's non-archived
// bookmarks with an add/update/remove action computed against the per-user/
// device "seen" ledger.
func (a *App) HandleAgentState(w http.ResponseWriter, r *http.Request) {
	deviceToken, readeckToken, ok := a.requireAgentUser(w, r, "/api/agent/state")
	if !ok {
		return
	}
	device := r.URL.Query().Get("device")
	if device == "" {
		http.Error(w, "Missing device parameter", http.StatusBadRequest)
		return
	}
	st := a.Store
	if st == nil {
		http.Error(w, "Store not configured", http.StatusInternalServerError)
		a.Logger.Errorf("Store is nil for /api/agent/state")
		return
	}

	readeckClient, err := a.newReadeckClient(readeckToken)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	isArchived := false
	bookmarks, err := readeckClient.GetBookmarks(ctx, "", &isArchived)
	if err != nil {
		http.Error(w, "Failed to fetch bookmarks", http.StatusInternalServerError)
		a.Logger.Errorf("agent/state: fetch bookmarks: %v", err)
		return
	}

	userHash := store.HashToken(readeckToken)
	seen, err := st.GetSeen(ctx, userHash, device)
	if err != nil {
		http.Error(w, "Failed to read seen ledger", http.StatusInternalServerError)
		a.Logger.Errorf("agent/state: read ledger: %v", err)
		return
	}
	seenByID := make(map[string]store.SeenRow, len(seen))
	for _, s := range seen {
		seenByID[s.BookmarkID] = s
	}

	current := make(map[string]*readeck.Bookmark, len(bookmarks))
	for i := range bookmarks {
		current[bookmarks[i].ID] = &bookmarks[i]
	}

	base := baseURLFromRequest(r)
	articles := make([]models.AgentStateArticle, 0, len(bookmarks))

	// add / update pass over current bookmarks, in stable order.
	currentIDs := make([]string, 0, len(current))
	for id := range current {
		currentIDs = append(currentIDs, id)
	}
	sort.Strings(currentIDs)
	for _, id := range currentIDs {
		bm := current[id]
		updated := bookmarkUpdated(bm)
		etag := etagForBookmark(bm)

		action := "add"
		if prev, ok := seenByID[id]; ok {
			if prev.Updated != updated {
				action = "update"
			} else {
				// unchanged: replay the stored action so the agent sees a
				// stable, idempotent instruction.
				action = prev.Action
				if action != "add" && action != "update" {
					action = "update"
				}
			}
		}
		_ = st.UpsertSeen(ctx, userHash, device, id, updated, action)

		articles = append(articles, models.AgentStateArticle{
			BookmarkID: id,
			Title:      bm.Title,
			Author:     strings.Join(bm.Authors, ", "),
			URL:        fmt.Sprintf("%s/api/kepub/%s?token=%s", base, url.PathEscape(id), url.QueryEscape(deviceToken)),
			Etag:       etag,
			Action:     action,
			Updated:    updated,
		})
	}

	// remove pass: seen bookmarks that are no longer among the current
	// non-archived set (archived or deleted) become "remove" and the ledger
	// row is dropped so the removal is emitted once.
	for _, s := range seen {
		if _, ok := current[s.BookmarkID]; ok {
			continue
		}
		articles = append(articles, models.AgentStateArticle{
			BookmarkID: s.BookmarkID,
			Action:     "remove",
		})
		_ = st.DeleteSeen(ctx, userHash, device, s.BookmarkID)
	}

	sort.Slice(articles, func(i, j int) bool { return articles[i].BookmarkID < articles[j].BookmarkID })

	resp := models.AgentStateResponse{Articles: articles, NextCursor: nil}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		a.Logger.Errorf("agent/state: encode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GET /api/kepub/{id}
// ---------------------------------------------------------------------------

// HandleKepubDownload serves the generated kepub for a bookmark
// (application/epub+zip), cached in the store keyed by bookmark_id + etag.
func (a *App) HandleKepubDownload(w http.ResponseWriter, r *http.Request) {
	_, readeckToken, ok := a.requireAgentUser(w, r, "/api/kepub")
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bookmark id", http.StatusBadRequest)
		return
	}
	st := a.Store
	if st == nil {
		http.Error(w, "Store not configured", http.StatusInternalServerError)
		a.Logger.Errorf("Store is nil for /api/kepub/%s", id)
		return
	}

	readeckClient, err := a.newReadeckClient(readeckToken)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	bm, err := readeckClient.GetBookmarkDetails(ctx, id)
	if err != nil {
		var apiErr *readeck.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bookmark not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to fetch bookmark", http.StatusInternalServerError)
		a.Logger.Errorf("kepub: fetch bookmark %s: %v", id, err)
		return
	}

	etag := bookmarkUpdated(bm)
	var articleHTML string
	if etag == "" {
		// No updated timestamp on the bookmark: derive the etag from the
		// article HTML (requires fetching it).
		articleHTML, err = readeckClient.GetBookmarkArticle(ctx, id)
		if err != nil {
			http.Error(w, "Failed to fetch article content", http.StatusInternalServerError)
			a.Logger.Errorf("kepub: fetch article %s: %v", id, err)
			return
		}
		etag = sha256Hex(articleHTML)
	}

	// Serve the cached artifact when it matches the current etag.
	cacheKey := kepubCacheKey(etag)
	if cached, err := st.GetKepub(ctx, id); err == nil && cached != nil && cached.Etag == cacheKey {
		a.serveKepub(w, id, cached.EPUB)
		return
	}

	if articleHTML == "" {
		articleHTML, err = readeckClient.GetBookmarkArticle(ctx, id)
		if err != nil {
			http.Error(w, "Failed to fetch article content", http.StatusInternalServerError)
			a.Logger.Errorf("kepub: fetch article %s: %v", id, err)
			return
		}
	}

	artifact, err := kepub.Build(articleHTML, kepubMetaFromBookmark(bm))
	if err != nil {
		http.Error(w, "Failed to generate kepub", http.StatusInternalServerError)
		a.Logger.Errorf("kepub: build %s: %v", id, err)
		return
	}
	if err := st.PutKepub(ctx, id, cacheKey, artifact.EPUB, string(artifact.SpanMapJSON)); err != nil {
		a.Logger.Warnf("kepub: cache write for %s: %v", id, err)
	}
	a.serveKepub(w, id, artifact.EPUB)
}

func (a *App) serveKepub(w http.ResponseWriter, id string, epub []byte) {
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".kepub.epub"))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(epub)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(epub); err != nil {
		a.Logger.Warnf("kepub: write response for %s: %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// POST /api/agent/annotations
// ---------------------------------------------------------------------------

// bookmarkInfo is per-request memoized data for one bookmark (details, served
// article HTML, span map).
type bookmarkInfo struct {
	bm    *readeck.Bookmark
	html  string
	spans []kepub.SpanRef
	err   error
}

// HandleAgentAnnotations ingests device highlight/note rows. Each item is
// processed independently: a failure never fails the batch.
func (a *App) HandleAgentAnnotations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, readeckToken, ok := a.requireAgentUser(w, r, "/api/agent/annotations")
	if !ok {
		return
	}
	st := a.Store
	if st == nil {
		http.Error(w, "Store not configured", http.StatusInternalServerError)
		a.Logger.Errorf("Store is nil for /api/agent/annotations")
		return
	}

	var req models.AgentAnnotationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Device == "" {
		http.Error(w, "Missing device in request body", http.StatusBadRequest)
		return
	}

	readeckClient, err := a.newReadeckClient(readeckToken)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	cache := make(map[string]*bookmarkInfo, len(req.Items))
	results := make([]models.AgentAnnotationResult, 0, len(req.Items))

	for _, item := range req.Items {
		res := models.AgentAnnotationResult{BookmarkRowID: item.BookmarkRowID}

		if item.BookmarkID == "" {
			res.Status = "error"
			res.Error = "empty bookmark_id"
			results = append(results, res)
			continue
		}

		info := a.agentBookmarkInfo(ctx, readeckClient, st, item.BookmarkID, cache)
		if info.err != nil {
			res.Status = "error"
			res.Error = info.err.Error()
			results = append(results, res)
			continue
		}

		mres := mapper.Map(info.spans, info.html, mapper.DeviceRange{
			StartPath:   item.StartPath,
			EndPath:     item.EndPath,
			StartOffset: item.StartOffset,
			EndOffset:   item.EndOffset,
			Text:        item.Text,
		})
		if mres.Range == nil {
			res.Status = "skipped"
			res.Error = mres.Reason
			results = append(results, res)
			continue
		}

		existing, err := readeckClient.GetBookmarkAnnotations(ctx, item.BookmarkID)
		if err != nil {
			res.Status = "error"
			res.Error = err.Error()
			results = append(results, res)
			continue
		}

		status, annotationID, ingestErr := a.ingestAnnotation(ctx, readeckClient, st, req.Device, item, mres.Range, existing)
		res.Status = status
		res.AnnotationID = annotationID
		if ingestErr != nil {
			res.Error = ingestErr.Error()
		}
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(models.AgentAnnotationsResponse{Results: results}); err != nil {
		a.Logger.Errorf("agent/annotations: encode response: %v", err)
	}
}

// agentBookmarkInfo memoizes, per request, the bookmark details, the served
// article HTML (also used for L2 text matching) and the span map. The kepub is
// only re-generated when the cache is missing or its etag is stale.
func (a *App) agentBookmarkInfo(ctx context.Context, rc *readeck.Client, st *store.Store, id string, cache map[string]*bookmarkInfo) *bookmarkInfo {
	if info, ok := cache[id]; ok {
		return info
	}
	info := &bookmarkInfo{}
	defer func() { cache[id] = info }()

	bm, err := rc.GetBookmarkDetails(ctx, id)
	if err != nil {
		var apiErr *readeck.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			info.err = fmt.Errorf("bookmark %s not found", id)
			return info
		}
		info.err = fmt.Errorf("fetch bookmark %s: %w", id, err)
		return info
	}
	info.bm = bm

	html, err := rc.GetBookmarkArticle(ctx, id)
	if err != nil {
		info.err = fmt.Errorf("fetch article %s: %w", id, err)
		return info
	}
	info.html = html

	// Same etag derivation as the kepub route (updated timestamp, else hash of
	// the article HTML) so both routes share one cache entry per bookmark.
	etag := bookmarkUpdated(bm)
	if etag == "" {
		etag = sha256Hex(html)
	}
	cacheKey := kepubCacheKey(etag)

	if cached, err := st.GetKepub(ctx, id); err == nil && cached != nil && cached.Etag == cacheKey {
		var spans []kepub.SpanRef
		if err := json.Unmarshal([]byte(cached.SpanMap), &spans); err == nil && len(spans) > 0 {
			info.spans = spans
			return info
		}
		// malformed or empty cache: fall through and regenerate.
	}

	artifact, err := kepub.Build(html, kepubMetaFromBookmark(bm))
	if err != nil {
		info.err = fmt.Errorf("build kepub for %s: %w", id, err)
		return info
	}
	info.spans = artifact.Spans
	if err := st.PutKepub(ctx, id, cacheKey, artifact.EPUB, string(artifact.SpanMapJSON)); err != nil {
		a.Logger.Warnf("agent/annotations: cache write for %s: %v", id, err)
	}
	return info
}

// ingestAnnotation applies the dedupe/conflict rules for one device row:
//
//  1. ledger has the row → unchanged unless date_modified changed → PATCH
//  2. no ledger row but the mapped range overlaps an existing annotation on
//     the same selector → PATCH that annotation and ledger-link it
//  3. otherwise → POST create
//
// The ledger is recorded after created/updated/overlap-handling. Colors are
// not mapped yet: everything is "yellow".
func (a *App) ingestAnnotation(ctx context.Context, rc *readeck.Client, st *store.Store, device string, item models.AgentAnnotationItem, mr *mapper.MappedRange, existing []readeck.Annotation) (string, string, error) {
	update := readeck.AnnotationUpdate{Color: "yellow", Note: truncateNote(item.Annotation)}

	ledger, err := st.GetAnnotationLedger(ctx, device, item.BookmarkRowID)
	if err != nil {
		return "error", "", err
	}
	if ledger != nil {
		if ledger.DateModified == item.DateModified {
			return "unchanged", ledger.AnnotationID, nil
		}
		if err := rc.UpdateAnnotation(ctx, item.BookmarkID, ledger.AnnotationID, update); err != nil {
			return "error", ledger.AnnotationID, err
		}
		_ = st.PutAnnotationLedger(ctx, device, item.BookmarkRowID, item.BookmarkID, ledger.AnnotationID, item.DateModified)
		return "updated", ledger.AnnotationID, nil
	}

	if hit := findOverlap(existing, mr); hit != nil {
		if err := rc.UpdateAnnotation(ctx, item.BookmarkID, hit.ID, update); err != nil {
			return "error", hit.ID, err
		}
		_ = st.PutAnnotationLedger(ctx, device, item.BookmarkRowID, item.BookmarkID, hit.ID, item.DateModified)
		return "updated", hit.ID, nil
	}

	created, err := rc.CreateAnnotation(ctx, item.BookmarkID, readeck.AnnotationCreate{
		StartSelector: mr.StartSelector,
		StartOffset:   mr.StartOffset,
		EndSelector:   mr.EndSelector,
		EndOffset:     mr.EndOffset,
		Color:         "yellow",
		Note:          truncateNote(item.Annotation),
	})
	if err != nil {
		return "error", "", err
	}
	_ = st.PutAnnotationLedger(ctx, device, item.BookmarkRowID, item.BookmarkID, created.ID, item.DateModified)
	return "created", created.ID, nil
}

// findOverlap returns the first existing annotation whose range overlaps the
// mapped range on the same selector (simple interval overlap). Readeck rejects
// overlapping ranges with 400, so a same-selector overlap is treated as
// "already highlighted" and absorbed.
func findOverlap(existing []readeck.Annotation, mr *mapper.MappedRange) *readeck.Annotation {
	for i := range existing {
		e := &existing[i]
		if e.StartSelector != mr.StartSelector || e.EndSelector != mr.EndSelector {
			continue
		}
		if e.StartOffset < mr.EndOffset && mr.StartOffset < e.EndOffset {
			return e
		}
	}
	return nil
}

// truncateNote trims and limits a device note to Readeck's 1024-rune bound.
func truncateNote(note string) string {
	note = strings.TrimSpace(note)
	runes := []rune(note)
	if len(runes) > 1024 {
		runes = runes[:1024]
	}
	return string(runes)
}
