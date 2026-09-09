package readeck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httputil" // Added import
	"net/url"
	"strconv"
	"strings"
	"time"

	"readeckobo/internal/logger"
)

const (
	defaultHTTPTimeout = 10 * time.Second
)

// Client represents a Readeck API client.
type Client struct {
	BaseURL    *url.URL
	AccessToken string
	HTTPClient *http.Client
	Logger     *logger.Logger // New field
}

// NewClient creates a new Readeck API client.
// APIError represents an error returned by the Readeck API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error: %s (status: %d)", e.Message, e.StatusCode)
}

func NewClient(baseURL string, accessToken string, logger *logger.Logger, httpClient *http.Client) (*Client, error) {
	parsedURL, err := url.ParseRequestURI(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}

	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: defaultHTTPTimeout,
		}
	}

	return &Client{
		BaseURL:    parsedURL,
		AccessToken: accessToken,
		HTTPClient: httpClient,
		Logger: logger,
	}, nil
}

// doRequest performs an HTTP request and decodes the response.
func (c *Client) doRequest(ctx context.Context, method, path string, queryParams url.Values, body any, v any) (string, error) {
	reqURL := c.BaseURL.JoinPath(path)
	reqURL.RawQuery = queryParams.Encode()

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	    if body != nil {
	        req.Header.Set("Content-Type", "application/json")
	    }
	
	    resp, err := c.HTTPClient.Do(req)
	    if err != nil {
	        return "", fmt.Errorf("failed to execute request: %w", err)
	    }
	    defer func() { _ = resp.Body.Close() }()
	
	    if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
	        return "", &APIError{StatusCode: resp.StatusCode, Message: resp.Status}
	    }
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return "", fmt.Errorf("failed to decode response body: %w", err)
		}
	}

	totalPages := strings.Join(resp.Header.Values("Link"), ",")
	return totalPages, nil
}

// doRequestRaw performs an HTTP request and returns the raw http.Response.
func (c *Client) doRequestRaw(ctx context.Context, method, path string, queryParams url.Values, body any) (*http.Response, error) {
	reqURL := c.BaseURL.JoinPath(path)
	reqURL.RawQuery = queryParams.Encode()

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Accept", "multipart/mixed") // Always accept multipart/mixed for Readeck API
	if body != nil {
		req.Header.Set("Content-Type", "application/json") // Ensure Content-Type is set for requests with a body
	}

	// Log the outgoing request for debugging
	dump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		c.Logger.Errorf("Failed to dump outgoing request: %v", err)
	} else {
		c.Logger.Debugf("Outgoing Readeck API Request:\n%s", dump)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	// Log the incoming response for debugging
	// dumpResp, err := httputil.DumpResponse(resp, true)
	// if err != nil {
	// 	c.Logger.Errorf("Failed to dump incoming response: %v", err)
	// } else {
	// 	c.Logger.Debugf("Incoming Readeck API Response:\n%s", dumpResp)
	// }

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() { _ = resp.Body.Close() }()
		respBodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("%s: %s", resp.Status, string(respBodyBytes))}
	}

	return resp, nil
}

// parseMultipartBookmarkResponse parses a multipart/mixed response containing bookmark details.
func parseMultipartBookmarkResponse(resp *http.Response, logger *logger.Logger) ([]Bookmark, error) {
	defer func() { _ = resp.Body.Close() }()

	logger.Debugf("Parsing multipart response. Overall Content-Type: %s", resp.Header.Get("Content-Type"))
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse Content-Type header: %w", err)
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("unexpected Content-Type: %s, expected multipart/mixed", mediaType)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("missing boundary in Content-Type header")
	}
	logger.Debugf("Multipart boundary: %s", boundary)

	mr := multipart.NewReader(resp.Body, boundary)

	var bookmarks []Bookmark
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			logger.Debugf("End of multipart parts.")
			break // No more parts
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read next part: %w", err)
		}

		partType := p.Header.Get("Type")
		partContentType := p.Header.Get("Content-Type")
		logger.Debugf("Processing multipart part. Type: %s, Content-Type: %s", partType, partContentType)

		if strings.HasPrefix(partContentType, "application/json") {
			partBytes, readErr := io.ReadAll(p)
			if readErr != nil {
				logger.Warnf("Failed to read JSON part content: %v", readErr)
				_ = p.Close()
				continue
			}
			logger.Debugf("Raw JSON part content: %s", string(partBytes))

			var bookmark Bookmark
			if err := json.Unmarshal(partBytes, &bookmark); err != nil {
				logger.Warnf("Failed to decode bookmark JSON part: %v, content: %s", err, string(partBytes))
				_ = p.Close()
				continue
			}
			logger.Debugf("Successfully decoded JSON part. Bookmark ID: %s", bookmark.ID)
			bookmarks = append(bookmarks, bookmark)
		} else {
			logger.Debugf("Skipping multipart part with Type: %s, Content-Type: %s", partType, partContentType)
		}
		_ = p.Close() // Close the part's body
	}

	return bookmarks, nil
}

// GetBookmarksSync fetches bookmark synchronization events.
func (c *Client) GetBookmarksSync(ctx context.Context, since *time.Time) ([]BookmarkSync, error) {
	queryParams := url.Values{}
	if since != nil {
		queryParams.Add("since", strconv.FormatInt(since.Unix(), 10))
	}

	var bookmarks []BookmarkSync
	_, err := c.doRequest(ctx, http.MethodGet, "/api/bookmarks/sync", queryParams, nil, &bookmarks)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bookmark syncs: %w", err)
	}

	return bookmarks, nil
}

// GetBookmarks fetches all bookmarks for a site (optionally filtered by
// is_archived), following Readeck's Link rel="next" pagination (limit/offset).
// Note: Readeck ignores the legacy `page` query param, so a page-based loop
// would silently rescan the first page forever.
func (c *Client) GetBookmarks(ctx context.Context, site string, isArchived *bool) ([]Bookmark, error) {
	var all []Bookmark
	offset := 0

	for {
		queryParams := url.Values{}
		if site != "" {
			queryParams.Add("site", site)
		}
		queryParams.Add("limit", "50")
		queryParams.Add("offset", strconv.Itoa(offset))
		if isArchived != nil {
			queryParams.Add("is_archived", strconv.FormatBool(*isArchived))
		}

		var bookmarks []Bookmark
		linkHeader, err := c.doRequest(ctx, http.MethodGet, "/api/bookmarks", queryParams, nil, &bookmarks)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch bookmarks: %w", err)
		}
		all = append(all, bookmarks...)

		nextOffset := nextOffsetFromLink(linkHeader)
		if nextOffset == nil {
			break
		}
		offset = *nextOffset
	}

	return all, nil
}

// GetVideos fetches every video bookmark (any archived state), following
// Readeck's Link rel="next" pagination (limit/offset). Readeck's `type`
// query param is the same discriminator behind its built-in "Videos"
// filter. Incremental Kobo syncs only see recently changed bookmarks, so
// this sweep lets them emit deletes for stale videos the device still
// holds.
func (c *Client) GetVideos(ctx context.Context) ([]Bookmark, error) {
	var all []Bookmark
	offset := 0

	for {
		queryParams := url.Values{}
		queryParams.Add("type", "video")
		queryParams.Add("limit", "50")
		queryParams.Add("offset", strconv.Itoa(offset))

		var bookmarks []Bookmark
		linkHeader, err := c.doRequest(ctx, http.MethodGet, "/api/bookmarks", queryParams, nil, &bookmarks)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch video bookmarks: %w", err)
		}
		all = append(all, bookmarks...)

		nextOffset := nextOffsetFromLink(linkHeader)
		if nextOffset == nil {
			break
		}
		offset = *nextOffset
	}

	return all, nil
}

// nextOffsetFromLink parses a Link header and returns the "offset" query
// parameter of the rel="next" link, or nil when there is no next page.
func nextOffsetFromLink(linkHeader string) *int {
	for _, part := range strings.Split(linkHeader, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start == -1 || end == -1 {
			continue
		}
		linkURL, err := url.Parse(strings.TrimSpace(part[start+1 : end]))
		if err != nil {
			return nil
		}
		offsetStr := linkURL.Query().Get("offset")
		if offsetStr == "" {
			return nil
		}
		offset, err := strconv.Atoi(offsetStr)
		if err != nil {
			return nil
		}
		return &offset
	}
	return nil
}

// GetBookmarkDetails fetches details for a single bookmark.
func (c *Client) GetBookmarkDetails(ctx context.Context, id string) (*Bookmark, error) {
	var bookmark Bookmark
	_, err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("/api/bookmarks/%s", id), nil, nil, &bookmark)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bookmark details: %w", err)
	}

	return &bookmark, nil
}

// GetBookmarkDetailsBatch fetches details for multiple bookmarks.
func (c *Client) SyncBookmarksContent(ctx context.Context, ids []string) (map[string]*Bookmark, error) {
	if len(ids) == 0 {
		return make(map[string]*Bookmark), nil
	}

	requestBody := map[string]any{
		"id":             ids,
		"resource_prefix": "%/img",
		"sort":            []string{"created"},
		"with_html":       false,
		"with_json":       true,
		"with_markdown":   false,
		"with_resources":  false,
	}

	c.Logger.Debugf("Fetching bookmark details via POST /api/bookmarks/sync for %d IDs", len(ids))

	// The response will be multipart/mixed, so we can't directly unmarshal into []Bookmark
	// We need to handle the multipart response manually.
	resp, err := c.doRequestRaw(ctx, http.MethodPost, "/api/bookmarks/sync", nil, requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch bookmark details in batch: %w", err)
	}

	// Parse multipart/mixed response
	bookmarks, err := parseMultipartBookmarkResponse(resp, c.Logger)
	if err != nil {
		return nil, fmt.Errorf("failed to parse multipart response: %w", err)
	}

	bookmarkMap := make(map[string]*Bookmark)
	for i := range bookmarks {
		bookmarkMap[bookmarks[i].ID] = &bookmarks[i]
	}

	return bookmarkMap, nil
}

// GetBookmarkArticle fetches the article content for a bookmark.
func (c *Client) GetBookmarkArticle(ctx context.Context, id string) (string, error) {
	reqURL := c.BaseURL.JoinPath(fmt.Sprintf("/api/bookmarks/%s/article", id))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, resp.Status)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(bodyBytes), nil
}

// UpdateBookmark updates a bookmark.
func (c *Client) UpdateBookmark(ctx context.Context, id string, updates map[string]any) error {
	path := fmt.Sprintf("/api/bookmarks/%s", id)
		_, err := c.doRequest(ctx, http.MethodPatch, path, nil, updates, nil)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			c.Logger.Infof("Bookmark with ID '%s' not found on Readeck server. Treating as a successful action for the Kobo client.", id)
			return nil // Treat "Not Found" as a success for the Kobo client
		}
		return fmt.Errorf("failed to update bookmark %s: %w", id, err)
	}
	return nil
}

// CreateBookmark creates a new bookmark.
func (c *Client) CreateBookmark(ctx context.Context, bookmarkURL string) error {
	body := map[string]string{"url": bookmarkURL}
	_, err := c.doRequest(ctx, http.MethodPost, "/api/bookmarks", nil, body, nil)
	if err != nil {
		return fmt.Errorf("failed to create bookmark: %w", err)
	}
	return nil
}
