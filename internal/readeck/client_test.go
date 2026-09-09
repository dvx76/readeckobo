package readeck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"readeckobo/internal/logger"
)

var testLogger = logger.New(logger.DEBUG)

func TestNewClient(t *testing.T) {
	client, err := NewClient("http://localhost:8080", "test-token", testLogger, nil)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if client.BaseURL.String() != "http://localhost:8080" {
		t.Errorf("Expected BaseURL to be http://localhost:8080, got %s", client.BaseURL.String())
	}
	if client.AccessToken != "test-token" {
		t.Errorf("Expected AccessToken to be test-token, got %s", client.AccessToken)
	}

	// This should now correctly return an error due to stricter URL parsing
	_, err = NewClient("invalid-url", "test-token", testLogger, nil)
	if err == nil {
		t.Error("Expected error for invalid URL, got nil")
	}
}

func TestBookmarkIsVideo(t *testing.T) {
	testCases := []struct {
		name     string
		bm       Bookmark
		expected bool
	}{
		{
			name:     "video bookmark",
			bm:       Bookmark{Type: "video"},
			expected: true,
		},
		{
			name:     "article bookmark",
			bm:       Bookmark{Type: "article"},
			expected: false,
		},
		{
			name:     "photo bookmark",
			bm:       Bookmark{Type: "photo"},
			expected: false,
		},
		{
			name:     "missing type (older Readeck)",
			bm:       Bookmark{},
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bm.IsVideo(); got != tc.expected {
				t.Errorf("IsVideo() = %v, want %v (type %q)", got, tc.expected, tc.bm.Type)
			}
		})
	}
}

func TestGetBookmarksSync(t *testing.T) {
	// Mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks/sync" {
			t.Errorf("Expected to request '/api/bookmarks/sync', got '%s'", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Expected Authorization header 'Bearer test-token', got '%s'", r.Header.Get("Authorization"))
		}

				mockResponse := []BookmarkSync{
			{ID: "1", Time: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), Type: "update"},
		}
		if err := json.NewEncoder(w).Encode(mockResponse); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	syncEvents, err := client.GetBookmarksSync(ctx, nil)
	if err != nil {
		t.Fatalf("GetBookmarksSync failed: %v", err)
	}
	if len(syncEvents) != 1 || syncEvents[0].ID != "1" {
		t.Errorf("Expected 1 sync event with ID '1', got %+v", syncEvents)
	}
}

func TestGetBookmarks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks" {
			t.Errorf("Expected to request '/api/bookmarks', got '%s'", r.URL.Path)
		}
		if r.URL.Query().Get("site") != "example.com" {
			t.Errorf("Expected site query parameter 'example.com', got '%s'", r.URL.Query().Get("site"))
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("Expected limit query parameter '50', got '%s'", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("offset") != "0" {
			t.Errorf("Expected offset query parameter '0', got '%s'", r.URL.Query().Get("offset"))
		}

		mockResponse := []Bookmark{
			{ID: "b1", Title: "Test Bookmark"},
		}
		// No Link header => no rel="next" => single page.
		if err := json.NewEncoder(w).Encode(mockResponse); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	bookmarks, err := client.GetBookmarks(ctx, "example.com", nil)
	if err != nil {
		t.Fatalf("GetBookmarks failed: %v", err)
	}
	if len(bookmarks) != 1 || bookmarks[0].ID != "b1" {
		t.Errorf("Expected 1 bookmark with ID 'b1', got %+v", bookmarks)
	}
}

func TestGetVideos(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks" {
			t.Errorf("Expected to request '/api/bookmarks', got '%s'", r.URL.Path)
		}
		if r.URL.Query().Get("type") != "video" {
			t.Errorf("Expected type query parameter 'video', got '%s'", r.URL.Query().Get("type"))
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("Expected limit query parameter '50', got '%s'", r.URL.Query().Get("limit"))
		}

		mockResponse := []Bookmark{
			{ID: "v1", Title: "Video One", Type: "video"},
			{ID: "v2", Title: "Video Two", Type: "video"},
		}
		// No Link header => no rel="next" => single page.
		if err := json.NewEncoder(w).Encode(mockResponse); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	videos, err := client.GetVideos(ctx)
	if err != nil {
		t.Fatalf("GetVideos failed: %v", err)
	}
	if len(videos) != 2 || videos[0].ID != "v1" || videos[1].ID != "v2" {
		t.Errorf("Expected 2 videos, got %+v", videos)
	}
	for _, v := range videos {
		if !v.IsVideo() {
			t.Errorf("Expected bookmark %s to be a video", v.ID)
		}
	}
}

func TestGetBookmarksPagination(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Path != "/api/bookmarks" {
			t.Errorf("Expected to request '/api/bookmarks', got '%s'", r.URL.Path)
		}
		offset := r.URL.Query().Get("offset")
		base := "https://" + r.Host + "/api/bookmarks?limit=50&offset=%d"
		switch requestCount {
		case 1:
			if offset != "0" {
				t.Errorf("Expected first request offset '0', got '%s'", offset)
			}
			w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"first\", <%s>; rel=\"next\", <%s>; rel=\"last\"", fmt.Sprintf(base, 0), fmt.Sprintf(base, 50), fmt.Sprintf(base, 50)))
			if err := json.NewEncoder(w).Encode([]Bookmark{{ID: "b1"}}); err != nil {
				t.Fatalf("Failed to encode response: %v", err)
			}
		case 2:
			if offset != "50" {
				t.Errorf("Expected second request offset '50', got '%s'", offset)
			}
			if err := json.NewEncoder(w).Encode([]Bookmark{{ID: "b2"}}); err != nil {
				t.Fatalf("Failed to encode response: %v", err)
			}
		default:
			t.Errorf("Unexpected extra request #%d", requestCount)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	bookmarks, err := client.GetBookmarks(ctx, "example.com", nil)
	if err != nil {
		t.Fatalf("GetBookmarks failed: %v", err)
	}
	if len(bookmarks) != 2 || bookmarks[0].ID != "b1" || bookmarks[1].ID != "b2" {
		t.Errorf("Expected both pages aggregated, got %+v", bookmarks)
	}
	if requestCount != 2 {
		t.Errorf("Expected exactly 2 requests, got %d", requestCount)
	}
}

// TestGetBookmarksPaginationMultiLineLink is a regression test for Readeck
// sending the Link header as separate header lines (previous, next, first,
// last) once offset > 0: resp.Header.Get("Link") only returns the first
// line (rel="previous"), which used to abort pagination after page 2.
func TestGetBookmarksPaginationMultiLineLink(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		offset := r.URL.Query().Get("offset")
		base := "https://" + r.Host + "/api/bookmarks?limit=50&offset=%d"
		switch requestCount {
		case 1:
			if offset != "0" {
				t.Errorf("Expected first request offset '0', got '%s'", offset)
			}
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"next\"", fmt.Sprintf(base, 50)))
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"first\"", fmt.Sprintf(base, 0)))
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"last\"", fmt.Sprintf(base, 100)))
		case 2:
			if offset != "50" {
				t.Errorf("Expected second request offset '50', got '%s'", offset)
			}
			// Readeck's real shape for offset >= 1 page: previous first,
			// next second, as separate header lines.
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"previous\"", fmt.Sprintf(base, 0)))
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"next\"", fmt.Sprintf(base, 100)))
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"first\"", fmt.Sprintf(base, 0)))
			w.Header().Add("Link", fmt.Sprintf("<%s>; rel=\"last\"", fmt.Sprintf(base, 100)))
		case 3:
			if offset != "100" {
				t.Errorf("Expected third request offset '100', got '%s'", offset)
			}
			// Last page: no rel="next" line at all.
		default:
			t.Errorf("Unexpected extra request #%d", requestCount)
		}
		if err := json.NewEncoder(w).Encode([]Bookmark{{ID: "b" + offset}}); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	bookmarks, err := client.GetBookmarks(ctx, "", nil)
	if err != nil {
		t.Fatalf("GetBookmarks failed: %v", err)
	}
	if len(bookmarks) != 3 ||
		bookmarks[0].ID != "b0" || bookmarks[1].ID != "b50" || bookmarks[2].ID != "b100" {
		t.Errorf("Expected all three pages aggregated, got %+v", bookmarks)
	}
	if requestCount != 3 {
		t.Errorf("Expected exactly 3 requests, got %d", requestCount)
	}
}

func TestGetBookmarkDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks/b1" {
			t.Errorf("Expected to request '/api/bookmarks/b1', got '%s'", r.URL.Path)
		}

		mockResponse := Bookmark{ID: "b1", Title: "Detailed Bookmark"}
		if err := json.NewEncoder(w).Encode(mockResponse); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	bookmark, err := client.GetBookmarkDetails(ctx, "b1")
	if err != nil {
		t.Fatalf("GetBookmarkDetails failed: %v", err)
	}
	if bookmark == nil || bookmark.ID != "b1" {
		t.Errorf("Expected bookmark with ID 'b1', got %+v", bookmark)
	}
}

func TestGetBookmarkArticle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks/b1/article" {
			t.Errorf("Expected to request '/api/bookmarks/b1/article', got '%s'", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write([]byte("<html><body><h1>Article Content</h1></body></html>")); err != nil {
			t.Fatalf("Failed to write response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	article, err := client.GetBookmarkArticle(ctx, "b1")
	if err != nil {
		t.Fatalf("GetBookmarkArticle failed: %v", err)
	}
	expectedArticle := "<html><body><h1>Article Content</h1></body></html>"
	if article != expectedArticle {
		t.Errorf("Expected article '%s', got '%s'", expectedArticle, article)
	}
}

func TestUpdateBookmark(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("Expected PATCH method, got %s", r.Method)
		}
		if r.URL.Path != "/api/bookmarks/b1" {
			t.Errorf("Expected to request '/api/bookmarks/b1', got '%s'", r.URL.Path)
		}

		var updates map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}
		if updates["is_archived"] != true {
			t.Errorf("Expected is_archived to be true, got %v", updates["is_archived"])
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	updates := map[string]interface{}{"is_archived": true}
	err := client.UpdateBookmark(ctx, "b1", updates)
	if err != nil {
		t.Fatalf("UpdateBookmark failed: %v", err)
	}
}

func TestUpdateBookmarkNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	updates := map[string]interface{}{"is_archived": true}
	err := client.UpdateBookmark(ctx, "nonexistent-id", updates)
	if err != nil {
		t.Errorf("Expected no error for 404 status, got %v", err)
	}
}

func TestCreateBookmark(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}
		if r.URL.Path != "/api/bookmarks" {
			t.Errorf("Expected to request '/api/bookmarks', got '%s'", r.URL.Path)
		}

		var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}
		if body["url"] != "http://example.com/new" {
			t.Errorf("Expected URL 'http://example.com/new', got '%s'", body["url"])
		}
		        w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
		
			client, _ := NewClient(server.URL, "test-token", testLogger, nil)
			ctx := context.Background()

	err := client.CreateBookmark(ctx, "http://example.com/new")
	if err != nil {
		t.Fatalf("CreateBookmark failed: %v", err)
	}
}

func TestGetBookmarksWithIsArchived(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bookmarks" {
			t.Errorf("Expected to request '/api/bookmarks', got '%s'", r.URL.Path)
		}
		if r.URL.Query().Get("site") != "example.com" {
			t.Errorf("Expected site query parameter 'example.com', got '%s'", r.URL.Query().Get("site"))
		}
		if r.URL.Query().Get("offset") != "0" {
			t.Errorf("Expected offset query parameter '0', got '%s'", r.URL.Query().Get("offset"))
		}
		if r.URL.Query().Get("is_archived") != "false" {
			t.Errorf("Expected is_archived query parameter 'false', got '%s'", r.URL.Query().Get("is_archived"))
		}

		mockResponse := []Bookmark{
			{ID: "b1", Title: "Test Bookmark"},
		}
		if err := json.NewEncoder(w).Encode(mockResponse); err != nil {
			t.Fatalf("Failed to encode response: %v", err)
		}
	}))
	defer server.Close()

	client, _ := NewClient(server.URL, "test-token", testLogger, nil)
	ctx := context.Background()

	isArchived := false
	bookmarks, err := client.GetBookmarks(ctx, "example.com", &isArchived)
	if err != nil {
		t.Fatalf("GetBookmarks failed: %v", err)
	}
	if len(bookmarks) != 1 || bookmarks[0].ID != "b1" {
		t.Errorf("Expected 1 bookmark with ID 'b1', got %+v", bookmarks)
	}
}
