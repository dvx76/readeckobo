package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Wire contract shared with the server side (stream D):
//
//	GET  {server}/api/agent/state?device=<serial>
//	     → {"articles":[{bookmark_id,title,author,url,etag,action,updated}], "next_cursor": null}
//	GET  {server}/api/kepub/{id}          → the .kepub.epub payload
//	POST {server}/api/agent/annotations
//	     {"device":..., "items":[{bookmark_id,bookmark_row_id,start_path,start_offset,
//	                              end_path,end_offset,text,annotation,type,
//	                              date_created,date_modified}]}
//	     → {"results":[{bookmark_row_id,status,annotation_id,error}]}
//
// Auth is `Authorization: Bearer <token>` on every request (the server also
// accepts ?token= but we never need it).
const userAgent = "readeckobo-agent/0.1"

// Article is one item of the state response.
type Article struct {
	BookmarkID string `json:"bookmark_id"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	URL        string `json:"url"` // absolute kepub download URL
	ETag       string `json:"etag"`
	Action     string `json:"action"` // add|update|remove
	Updated    string `json:"updated"`
}

type stateResponse struct {
	Articles   []Article `json:"articles"`
	NextCursor *string   `json:"next_cursor"` // non-null pagination is not implemented in v1
}

// uploadItem is one annotation payload entry (exact contract field names).
type uploadItem struct {
	BookmarkID    string `json:"bookmark_id"`
	BookmarkRowID string `json:"bookmark_row_id"`
	StartPath     string `json:"start_path"`
	StartOffset   int    `json:"start_offset"`
	EndPath       string `json:"end_path"`
	EndOffset     int    `json:"end_offset"`
	Text          string `json:"text"`
	Annotation    string `json:"annotation"`
	Type          string `json:"type"`
	DateCreated   string `json:"date_created"`
	DateModified  string `json:"date_modified"`
}

type annotationRequest struct {
	Device string       `json:"device"`
	Items  []uploadItem `json:"items"`
}

type annotationResult struct {
	BookmarkRowID string `json:"bookmark_row_id"`
	Status        string `json:"status"` // created|updated|unchanged|skipped (anything else = error)
	AnnotationID  string `json:"annotation_id"`
	Error         string `json:"error"`
}

type annotationResponse struct {
	Results []annotationResult `json:"results"`
}

// apiError carries the HTTP status and a truncated body snippet.
type apiError struct {
	method string
	url    string
	status int
	body   string
}

func (e *apiError) Error() string {
	msg := e.body
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Sprintf("%s %s: unexpected status %d (%s)", e.method, e.url, e.status, msg)
}

// apiClient talks to the readeckobo server.
type apiClient struct {
	base  string // server URL, no trailing slash
	token string
	httpc *http.Client
}

func newAPIClient(cfg *Config) *apiClient {
	return &apiClient{
		base:  cfg.ServerURL,
		token: cfg.Token,
	}
}

func (c *apiClient) do(ctx context.Context, method, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The HTTP deadline lives on the client (http.Client.Timeout, set at
	// agent construction): a per-request context.WithTimeout here would
	// cancel the request context the moment Do() returns — before the
	// caller reads the response body — turning every body read into
	// "context canceled". Client.Timeout covers the whole exchange
	// (headers + body reads) and is canceled by net/http only when the
	// deadline actually expires.
	return c.httpc.Do(req)
}

// checkStatus wraps a non-2xx response into an apiError with a body snippet.
func checkStatus(method, rawURL string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &apiError{method: method, url: rawURL, status: resp.StatusCode, body: strings.TrimSpace(string(body))}
}

// fetchState implements GET {server}/api/agent/state?device=<serial>.
func (c *apiClient) fetchState(ctx context.Context, device string) ([]Article, error) {
	rawURL := c.base + "/api/agent/state?device=" + url.QueryEscape(device)
	resp, err := c.do(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("GET state: %w", err)
	}
	if err := checkStatus(http.MethodGet, rawURL, resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var sr stateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sr); err != nil {
		return nil, fmt.Errorf("GET state: decoding response: %w", err)
	}
	if sr.NextCursor != nil && *sr.NextCursor != "" {
		// v1 contract returns next_cursor: null; if the server ever pages,
		// we must implement continuation before trusting a partial list.
		return nil, fmt.Errorf("state response has next_cursor %q: pagination is not implemented in the agent v1", *sr.NextCursor)
	}
	return sr.Articles, nil
}

// fetchKepub downloads the kepub payload from the absolute URL given by the
// state response (GET {server}/api/kepub/{id}).
func (c *apiClient) fetchKepub(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("kepub URL %q is not an http(s) URL", rawURL)
	}
	resp, err := c.do(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("GET kepub: %w", err)
	}
	if err := checkStatus(http.MethodGet, rawURL, resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30)) // 1 GiB ceiling, kepubs are ≪ that
	if err != nil {
		return nil, fmt.Errorf("GET kepub: reading body: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("GET kepub: empty body from %s", rawURL)
	}
	return data, nil
}

// postAnnotations implements POST {server}/api/agent/annotations. items must
// not be empty. It returns the results keyed by bookmark row id.
func (c *apiClient) postAnnotations(ctx context.Context, device string, items []uploadItem) (map[string]annotationResult, error) {
	if len(items) == 0 {
		return map[string]annotationResult{}, nil
	}
	body, err := json.Marshal(annotationRequest{Device: device, Items: items})
	if err != nil {
		return nil, err
	}
	rawURL := c.base + "/api/agent/annotations"
	resp, err := c.do(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("POST annotations: %w", err)
	}
	if err := checkStatus(http.MethodPost, rawURL, resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ar annotationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ar); err != nil {
		return nil, fmt.Errorf("POST annotations: decoding response: %w", err)
	}
	out := make(map[string]annotationResult, len(ar.Results))
	for _, r := range ar.Results {
		out[r.BookmarkRowID] = r
	}
	return out, nil
}
