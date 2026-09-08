package models

// Agent models are the wire contract for the on-device agent
// (stream E). See docs/research/mapping-and-endpoints.md.
//
// Auth on all /api/agent/* and /api/kepub/* routes is either
// `Authorization: Bearer <device-token>` or `?token=<device-token>`, where the
// device token matches a users[].token in config.

// AgentStateArticle is one entry of the agent state feed
// (GET /api/agent/state?device=<serial>).
type AgentStateArticle struct {
	BookmarkID string `json:"bookmark_id"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	URL        string `json:"url"` // absolute URL to /api/kepub/<id> (carries ?token=)
	Etag       string `json:"etag"`
	Action     string `json:"action"`  // "add" | "update" | "remove"
	Updated    string `json:"updated"` // RFC3339
}

// AgentStateResponse is the state feed response. next_cursor stays null for
// v1: every call returns the full list.
type AgentStateResponse struct {
	Articles   []AgentStateArticle `json:"articles"`
	NextCursor *string             `json:"next_cursor"`
}

// AgentAnnotationItem is one device highlight/note to ingest
// (POST /api/agent/annotations). start_path/end_path carry the kepub span id
// in a fragment ("OEBPS/xhtml/ch001.xhtml#kobo.2.1"); offsets are rune offsets
// within the respective span's text.
type AgentAnnotationItem struct {
	BookmarkID    string `json:"bookmark_id"`
	BookmarkRowID string `json:"bookmark_row_id"`
	StartPath     string `json:"start_path"`
	StartOffset   int    `json:"start_offset"`
	EndPath       string `json:"end_path"`
	EndOffset     int    `json:"end_offset"`
	Text          string `json:"text"`
	Annotation    string `json:"annotation"`
	Type          string `json:"type"` // "highlight" | "note"
	DateCreated   string `json:"date_created"`
	DateModified  string `json:"date_modified"`
}

// AgentAnnotationsRequest is the POST /api/agent/annotations body.
type AgentAnnotationsRequest struct {
	Device string                `json:"device"`
	Items  []AgentAnnotationItem `json:"items"`
}

// AgentAnnotationResult is one per-item outcome.
type AgentAnnotationResult struct {
	BookmarkRowID string `json:"bookmark_row_id"`
	Status        string `json:"status"` // "created" | "updated" | "unchanged" | "skipped" | "error"
	AnnotationID  string `json:"annotation_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

// AgentAnnotationsResponse is the POST /api/agent/annotations response.
type AgentAnnotationsResponse struct {
	Results []AgentAnnotationResult `json:"results"`
}
