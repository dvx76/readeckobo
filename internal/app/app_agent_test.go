package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"readeckobo/internal/config"
	"readeckobo/internal/models"
	"readeckobo/internal/readeck"
	"readeckobo/internal/store"
)

// ---------------------------------------------------------------------------
// In-memory Readeck mock (bookmarks, article, annotations CRUD)
// ---------------------------------------------------------------------------

// readeckMock is an in-memory fake of the Readeck API surface the agent
// handlers use. It enforces Readeck's overlap rule on annotation create.
type readeckMock struct {
	mu sync.Mutex

	bookmarks []readeck.Bookmark
	article   string

	annotations    map[string][]readeck.Annotation // by bookmark id
	nextAnnotation int

	articleFetches int
	createCalls    int
	patchCalls     int
	createdBody    readeck.AnnotationCreate
	patchedBody    readeck.AnnotationUpdate
}

func newReadeckMock(article string) *readeckMock {
	return &readeckMock{article: article, annotations: map[string][]readeck.Annotation{}}
}

func (m *readeckMock) setBookmarks(bookmarks ...readeck.Bookmark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bookmarks = bookmarks
}

func (m *readeckMock) bookmarkByID(id string) *readeck.Bookmark {
	for i := range m.bookmarks {
		if m.bookmarks[i].ID == id {
			return &m.bookmarks[i]
		}
	}
	return nil
}

// overlap returns whether the create range overlaps an existing annotation on
// the same selector (Readeck's geometric dedup).
func (m *readeckMock) overlap(bookmarkID string, body readeck.AnnotationCreate) bool {
	for _, a := range m.annotations[bookmarkID] {
		if a.StartSelector != body.StartSelector || a.EndSelector != body.EndSelector {
			continue
		}
		if a.StartOffset < body.EndOffset && body.StartOffset < a.EndOffset {
			return true
		}
	}
	return false
}

func (m *readeckMock) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		switch {
		case r.URL.Path == "/api/bookmarks":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if got := r.URL.Query().Get("is_archived"); got != "false" {
				http.Error(w, "mock: expected is_archived=false, got "+got, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(m.bookmarks)

		case strings.HasSuffix(r.URL.Path, "/article"):
			m.articleFetches++
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, m.article)

		case strings.HasSuffix(r.URL.Path, "/annotations"):
			bookmarkID := bookmarkIDFromPath(r.URL.Path, "/annotations")
			switch r.Method {
			case http.MethodGet:
				list := m.annotations[bookmarkID]
				if list == nil {
					list = []readeck.Annotation{}
				}
				_ = json.NewEncoder(w).Encode(list)
			case http.MethodPost:
				var body readeck.AnnotationCreate
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "bad body", http.StatusBadRequest)
					return
				}
				if m.overlap(bookmarkID, body) {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"status": 400, "message": "overlapping annotation"})
					return
				}
				m.nextAnnotation++
				a := readeck.Annotation{
					ID:            fmt.Sprintf("ann-%d", m.nextAnnotation),
					StartSelector: body.StartSelector,
					StartOffset:   body.StartOffset,
					EndSelector:   body.EndSelector,
					EndOffset:     body.EndOffset,
					Color:         body.Color,
					Note:          body.Note,
					Created:       time.Now(),
				}
				m.annotations[bookmarkID] = append(m.annotations[bookmarkID], a)
				m.createCalls++
				m.createdBody = body
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(a)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}

		case strings.Contains(r.URL.Path, "/annotations/"):
			if r.Method != http.MethodPatch {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/bookmarks/"), "/annotations/", 2)
			if len(parts) != 2 {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			bookmarkID := parts[0]
			annotationID := parts[1]
			var body readeck.AnnotationUpdate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			list := m.annotations[bookmarkID]
			found := false
			for i := range list {
				if list[i].ID == annotationID {
					list[i].Color = body.Color
					list[i].Note = body.Note
					found = true
				}
			}
			if !found {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			m.patchCalls++
			m.patchedBody = body
			_ = json.NewEncoder(w).Encode(map[string]any{
				"annotations": list,
				"updated":     time.Now().Format(time.RFC3339Nano),
			})

		default:
			// GET /api/bookmarks/{id} — details.
			id := strings.TrimPrefix(r.URL.Path, "/api/bookmarks/")
			bm := m.bookmarkByID(id)
			if bm == nil {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(bm)
		}
	})
}

func bookmarkIDFromPath(path, suffix string) string {
	p := strings.TrimPrefix(path, "/api/bookmarks/")
	return strings.TrimSuffix(p, suffix)
}

// ---------------------------------------------------------------------------
// Test scaffolding
// ---------------------------------------------------------------------------

const agentDeviceToken = "agent-device-token"

func fixtureArticle(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../kepub/testdata/readeck-article.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func newAgentApp(t *testing.T, mock *readeckMock) (*App, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	app := NewApp(
		WithConfig(&config.Config{
			Users:   []config.User{{Token: agentDeviceToken, ReadeckAccessToken: mockPlaintextReadeckToken}},
			Readeck: config.ConfigReadeck{Host: srv.URL},
		}),
		WithLogger(testLogger),
		WithReadeckHTTPClient(srv.Client()),
		WithStore(st),
	)
	return app, srv
}

func stateRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/state?device=dev-1", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func decodeState(t *testing.T, rr *httptest.ResponseRecorder) models.AgentStateResponse {
	t.Helper()
	var resp models.AgentStateResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode state response: %v (body %s)", err, rr.Body.String())
	}
	return resp
}

func decodeAnnotations(t *testing.T, rr *httptest.ResponseRecorder) models.AgentAnnotationsResponse {
	t.Helper()
	var resp models.AgentAnnotationsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode annotations response: %v (body %s)", err, rr.Body.String())
	}
	return resp
}

// ---------------------------------------------------------------------------
// State feed
// ---------------------------------------------------------------------------

func TestHandleAgentState(t *testing.T) {
	t1 := time.Date(2026, 9, 1, 8, 0, 0, 123000000, time.UTC)
	t2 := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(
		readeck.Bookmark{ID: "bm-1", Title: "Article One", Authors: []string{"Ada Example"}, Updated: t1, IsArchived: false},
		readeck.Bookmark{ID: "bm-2", Title: "Article Two", Updated: t2, IsArchived: false},
	)
	app, _ := newAgentApp(t, mock)
	fingerprint := ""

	t.Run("first call: both add", func(t *testing.T) {
		rr := httptest.NewRecorder()
		app.HandleAgentState(rr, stateRequest(agentDeviceToken))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
		resp := decodeState(t, rr)
		if len(resp.Articles) != 2 {
			t.Fatalf("expected 2 articles, got %d: %+v", len(resp.Articles), resp.Articles)
		}
		if resp.NextCursor != nil {
			t.Errorf("next_cursor must be null for v1, got %v", *resp.NextCursor)
		}
		byID := map[string]models.AgentStateArticle{}
		for _, a := range resp.Articles {
			byID[a.BookmarkID] = a
		}
		for _, id := range []string{"bm-1", "bm-2"} {
			a := byID[id]
			if a.Action != "add" {
				t.Errorf("%s action = %q, want add", id, a.Action)
			}
			if a.Etag == "" || a.Updated == "" {
				t.Errorf("%s missing etag/updated: %+v", id, a)
			}
			wantURL := "http://example.com/api/kepub/" + id + "?token=" + url.QueryEscape(agentDeviceToken)
			if !strings.HasSuffix(a.URL, "/api/kepub/"+id+"?token="+url.QueryEscape(agentDeviceToken)) {
				t.Errorf("%s url = %q, want suffix %q", id, a.URL, wantURL)
			}
		}
		if got := byID["bm-1"].Etag; got != t1.Format(time.RFC3339Nano) {
			t.Errorf("bm-1 etag = %q, want updated timestamp %q", got, t1.Format(time.RFC3339Nano))
		}
		fingerprint = byID["bm-1"].Etag
	})

	t.Run("second call unchanged: actions replayed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		app.HandleAgentState(rr, stateRequest(agentDeviceToken))
		resp := decodeState(t, rr)
		byID := map[string]models.AgentStateArticle{}
		for _, a := range resp.Articles {
			byID[a.BookmarkID] = a
		}
		if byID["bm-1"].Action != "add" || byID["bm-2"].Action != "add" {
			t.Errorf("actions after unchanged: %+v", resp.Articles)
		}
	})

	t.Run("query param token works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/agent/state?device=dev-1&token="+agentDeviceToken, nil)
		rr := httptest.NewRecorder()
		app.HandleAgentState(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("updated changed: update", func(t *testing.T) {
		bm := mock.bookmarkByID("bm-1")
		bm.Updated = t3
		rr := httptest.NewRecorder()
		app.HandleAgentState(rr, stateRequest(agentDeviceToken))
		resp := decodeState(t, rr)
		var bm1 *models.AgentStateArticle
		for i := range resp.Articles {
			if resp.Articles[i].BookmarkID == "bm-1" {
				bm1 = &resp.Articles[i]
			}
		}
		if bm1 == nil || bm1.Action != "update" {
			t.Fatalf("bm-1 action = %+v, want update", bm1)
		}
		if bm1.Etag == fingerprint {
			t.Error("etag should change when updated changes")
		}
		for i := range resp.Articles {
			if resp.Articles[i].BookmarkID == "bm-2" && resp.Articles[i].Action != "add" {
				t.Errorf("bm-2 should stay add, got %q", resp.Articles[i].Action)
			}
		}
	})

	t.Run("archived now: remove, then gone", func(t *testing.T) {
		mock.setBookmarks(
			readeck.Bookmark{ID: "bm-2", Title: "Article Two", Updated: t2, IsArchived: false},
		)
		rr := httptest.NewRecorder()
		app.HandleAgentState(rr, stateRequest(agentDeviceToken))
		resp := decodeState(t, rr)
		var removed *models.AgentStateArticle
		for i := range resp.Articles {
			if resp.Articles[i].BookmarkID == "bm-1" {
				removed = &resp.Articles[i]
			}
		}
		if removed == nil || removed.Action != "remove" {
			t.Fatalf("bm-1 should be remove, got %+v", resp.Articles)
		}
		if removed.URL != "" {
			t.Errorf("removed item should not carry a download url, got %q", removed.URL)
		}

		// The removal is emitted once: the ledger row is dropped.
		rr2 := httptest.NewRecorder()
		app.HandleAgentState(rr2, stateRequest(agentDeviceToken))
		resp2 := decodeState(t, rr2)
		for _, a := range resp2.Articles {
			if a.BookmarkID == "bm-1" {
				t.Errorf("bm-1 should be gone from the feed after remove, got %+v", resp2.Articles)
			}
		}
	})
}

func TestHandleAgentStateUnauthorized(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(readeck.Bookmark{ID: "bm-1", Title: "T", Updated: time.Now(), IsArchived: false})
	app, _ := newAgentApp(t, mock)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"unknown token", "not-a-device-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			app.HandleAgentState(rr, stateRequest(tc.token))
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestHandleAgentStateMissingDevice(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(readeck.Bookmark{ID: "bm-1", Title: "T", Updated: time.Now(), IsArchived: false})
	app, _ := newAgentApp(t, mock)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agent/state", nil)
	req.Header.Set("Authorization", "Bearer "+agentDeviceToken)
	app.HandleAgentState(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Kepub download
// ---------------------------------------------------------------------------

func TestHandleKepubDownload(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(readeck.Bookmark{
		ID: "bm-1", Title: "Test Article", Authors: []string{"Ada Example"},
		Updated: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC), IsArchived: false,
		URL: "https://example.com/article",
	})
	app, _ := newAgentApp(t, mock)

	req := httptest.NewRequest(http.MethodGet, "/api/kepub/bm-1", nil)
	req.SetPathValue("id", "bm-1")
	req.Header.Set("Authorization", "Bearer "+agentDeviceToken)
	rr := httptest.NewRecorder()
	app.HandleKepubDownload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/epub+zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := rr.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "bm-1.kepub.epub") || !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// Valid EPUB zip: mimetype stored first.
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("response is not a zip: %v", err)
	}
	if len(zr.File) != 6 || zr.File[0].Name != "mimetype" {
		t.Errorf("unexpected zip layout: %d entries, first %q", len(zr.File), zr.File[0].Name)
	}

	// Second call is served from the cache: no article fetch, no re-build.
	rr2 := httptest.NewRecorder()
	app.HandleKepubDownload(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call status = %d", rr2.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), rr2.Body.Bytes()) {
		t.Error("cached kepub differs from the first response")
	}
	if mock.articleFetches != 1 {
		t.Errorf("article fetched %d times, want 1 (cache hit)", mock.articleFetches)
	}
}

func TestHandleKepubDownloadNotFound(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t)) // no bookmarks
	app, _ := newAgentApp(t, mock)

	req := httptest.NewRequest(http.MethodGet, "/api/kepub/nope", nil)
	req.SetPathValue("id", "nope")
	req.Header.Set("Authorization", "Bearer "+agentDeviceToken)
	rr := httptest.NewRecorder()
	app.HandleKepubDownload(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleKepubDownloadUnauthorized(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	app, _ := newAgentApp(t, mock)

	req := httptest.NewRequest(http.MethodGet, "/api/kepub/bm-1", nil)
	rr := httptest.NewRecorder()
	app.HandleKepubDownload(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Annotation ingress
// ---------------------------------------------------------------------------

func testBookmark(updated time.Time) readeck.Bookmark {
	return readeck.Bookmark{
		ID: "bm-1", Title: "Test Article", Authors: []string{"Ada Example"},
		Updated: updated, IsArchived: false,
		URL: "https://example.com/article",
	}
}

func annotationItem(overrides map[string]any) models.AgentAnnotationItem {
	item := models.AgentAnnotationItem{
		BookmarkID:    "bm-1",
		BookmarkRowID: "row-1",
		StartPath:     "OEBPS/xhtml/ch001.xhtml#kobo.1.1",
		StartOffset:   0,
		EndPath:       "OEBPS/xhtml/ch001.xhtml#kobo.1.1",
		EndOffset:     5,
		Text:          "ALPHA",
		Annotation:    "",
		Type:          "highlight",
		DateCreated:   "2026-09-01T00:00:00Z",
		DateModified:  "2026-09-01T00:00:00Z",
	}
	for k, v := range overrides {
		switch k {
		case "bookmark_row_id":
			item.BookmarkRowID = v.(string)
		case "date_modified":
			item.DateModified = v.(string)
		case "start_path":
			item.StartPath = v.(string)
		case "end_path":
			item.EndPath = v.(string)
		case "start_offset":
			item.StartOffset = v.(int)
		case "end_offset":
			item.EndOffset = v.(int)
		case "text":
			item.Text = v.(string)
		case "annotation":
			item.Annotation = v.(string)
		case "bookmark_id":
			item.BookmarkID = v.(string)
		}
	}
	return item
}

func postAnnotations(t *testing.T, app *App, items ...models.AgentAnnotationItem) models.AgentAnnotationsResponse {
	t.Helper()
	body, err := json.Marshal(models.AgentAnnotationsRequest{Device: "dev-1", Items: items})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/annotations", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agentDeviceToken)
	rr := httptest.NewRecorder()
	app.HandleAgentAnnotations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	return decodeAnnotations(t, rr)
}

func TestHandleAgentAnnotationsHappyPath(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(testBookmark(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)))
	app, _ := newAgentApp(t, mock)

	resp := postAnnotations(t, app, annotationItem(nil))
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %+v", resp.Results)
	}
	res := resp.Results[0]
	if res.Status != "created" || res.AnnotationID == "" || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The POSTed range was computed with L1: alpha paragraph, runes 0..5.
	body := mock.createdBody
	if body.StartSelector != `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']` ||
		body.EndSelector != body.StartSelector {
		t.Errorf("selectors = %q / %q", body.StartSelector, body.EndSelector)
	}
	if body.StartOffset != 0 || body.EndOffset != 5 {
		t.Errorf("offsets = %d..%d, want 0..5", body.StartOffset, body.EndOffset)
	}
	if body.Color != "yellow" {
		t.Errorf("color = %q, want yellow", body.Color)
	}
	if mock.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", mock.createCalls)
	}

	t.Run("idempotent re-post: unchanged", func(t *testing.T) {
		resp := postAnnotations(t, app, annotationItem(nil))
		if resp.Results[0].Status != "unchanged" {
			t.Errorf("re-post status = %q, want unchanged (%+v)", resp.Results[0].Status, resp.Results[0])
		}
		if resp.Results[0].AnnotationID != res.AnnotationID {
			t.Errorf("annotation id = %q, want %q", resp.Results[0].AnnotationID, res.AnnotationID)
		}
		if mock.createCalls != 1 {
			t.Errorf("createCalls = %d after re-post, want 1", mock.createCalls)
		}
	})

	t.Run("modified row: updated via PATCH", func(t *testing.T) {
		resp := postAnnotations(t, app, annotationItem(map[string]any{
			"date_modified": "2026-09-02T00:00:00Z",
			"annotation":    "an edited note",
		}))
		if resp.Results[0].Status != "updated" {
			t.Errorf("status = %q, want updated (%+v)", resp.Results[0].Status, resp.Results[0])
		}
		if resp.Results[0].AnnotationID != res.AnnotationID {
			t.Errorf("annotation id = %q, want %q", resp.Results[0].AnnotationID, res.AnnotationID)
		}
		if mock.patchCalls != 1 {
			t.Errorf("patchCalls = %d, want 1", mock.patchCalls)
		}
		if mock.patchedBody.Color != "yellow" || mock.patchedBody.Note != "an edited note" {
			t.Errorf("patched body = %+v", mock.patchedBody)
		}
	})
}

func TestHandleAgentAnnotationsOverlapAbsorbed(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(testBookmark(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)))
	app, _ := newAgentApp(t, mock)

	// First row: [0,5) in the alpha paragraph.
	r1 := postAnnotations(t, app, annotationItem(nil))
	firstID := r1.Results[0].AnnotationID

	// Second row overlaps the same range on the same selector: it must be
	// absorbed via PATCH (Readeck would reject the POST with 400), and the
	// ledger links it to the same annotation.
	resp := postAnnotations(t, app, annotationItem(map[string]any{
		"bookmark_row_id": "row-2",
		"start_offset":    1,
		"end_offset":      3,
	}))
	res := resp.Results[0]
	if res.Status != "updated" {
		t.Fatalf("status = %q, want updated (%+v)", res.Status, res)
	}
	if res.AnnotationID != firstID {
		t.Errorf("annotation id = %q, want %q (overlap absorbed)", res.AnnotationID, firstID)
	}
	if mock.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no new annotation)", mock.createCalls)
	}
	if mock.patchCalls != 1 {
		t.Errorf("patchCalls = %d, want 1", mock.patchCalls)
	}

	// Re-posting row-2 unchanged → unchanged.
	resp2 := postAnnotations(t, app, annotationItem(map[string]any{
		"bookmark_row_id": "row-2",
		"start_offset":    1,
		"end_offset":      3,
	}))
	if resp2.Results[0].Status != "unchanged" {
		t.Errorf("row-2 re-post status = %q, want unchanged", resp2.Results[0].Status)
	}
}

func TestHandleAgentAnnotationsL2AndSkipped(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(testBookmark(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)))
	app, _ := newAgentApp(t, mock)

	// L2 fallback: unknown span id, but the text exists in the article
	// (CHARLIE paragraph). First occurrence wins → created via L2 selector.
	resp := postAnnotations(t, app, annotationItem(map[string]any{
		"bookmark_row_id": "row-l2",
		"start_path":      "OEBPS/xhtml/ch001.xhtml#kobo.99.1",
		"end_path":        "OEBPS/xhtml/ch001.xhtml#kobo.99.1",
		"start_offset":    0,
		"end_offset":      0,
		"text":            "CHARLIE delta echo foxtrot golf hotel india juliet kilo lima.",
	}))
	if resp.Results[0].Status != "created" {
		t.Fatalf("L2 item status = %q, want created (%+v)", resp.Results[0].Status, resp.Results[0])
	}
	body := mock.createdBody
	if body.StartSelector != "section[1]/article[1]/p[3]" {
		t.Errorf("L2 selector = %q, want section[1]/article[1]/p[3]", body.StartSelector)
	}

	// Unmapped: unknown span and missing text → skipped with a reason.
	resp = postAnnotations(t, app, annotationItem(map[string]any{
		"bookmark_row_id": "row-skip",
		"start_path":      "OEBPS/xhtml/ch001.xhtml#kobo.99.1",
		"end_path":        "OEBPS/xhtml/ch001.xhtml#kobo.99.1",
		"text":            "this text does not exist anywhere in the article at all",
	}))
	if resp.Results[0].Status != "skipped" || resp.Results[0].Error == "" {
		t.Errorf("unmapped result = %+v, want skipped with reason", resp.Results[0])
	}
	if mock.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (only the L2 item creates)", mock.createCalls)
	}
}

func TestHandleAgentAnnotationsBatchIsolation(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	mock.setBookmarks(testBookmark(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)))
	app, _ := newAgentApp(t, mock)

	// One good item, one unmapped, one unknown bookmark, one on a missing
	// bookmark (404). The batch must not fail as a whole.
	resp := postAnnotations(t, app,
		annotationItem(nil),
		annotationItem(map[string]any{
			"bookmark_row_id": "row-bad-1",
			"start_path":      "OEBPS/xhtml/ch001.xhtml#kobo.99.1",
			"text":            "no such text anywhere in the fixture at all",
		}),
		annotationItem(map[string]any{"bookmark_row_id": "row-bad-2", "bookmark_id": "unknown-book"}),
	)
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %+v", resp.Results)
	}
	byRow := map[string]models.AgentAnnotationResult{}
	for _, r := range resp.Results {
		byRow[r.BookmarkRowID] = r
	}
	if byRow["row-1"].Status != "created" {
		t.Errorf("good item: %+v", byRow["row-1"])
	}
	if byRow["row-bad-1"].Status != "skipped" {
		t.Errorf("unmapped item: %+v", byRow["row-bad-1"])
	}
	if byRow["row-bad-2"].Status != "error" {
		t.Errorf("unknown bookmark item: %+v", byRow["row-bad-2"])
	}
	// The good item was ingested despite the bad ones.
	if mock.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", mock.createCalls)
	}

	// A single batch touching one bookmark only fetches the article once.
	if mock.articleFetches != 1 {
		t.Errorf("articleFetches = %d, want 1 (memoized per request)", mock.articleFetches)
	}
}

func TestHandleAgentAnnotationsUnauthorized(t *testing.T) {
	mock := newReadeckMock(fixtureArticle(t))
	app, _ := newAgentApp(t, mock)

	body, _ := json.Marshal(models.AgentAnnotationsRequest{Device: "dev-1", Items: []models.AgentAnnotationItem{annotationItem(nil)}})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/annotations", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer not-a-device-token")
	rr := httptest.NewRecorder()
	app.HandleAgentAnnotations(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
