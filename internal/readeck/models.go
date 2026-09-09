package readeck

import (
	"time"
)

type BookmarkSync struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Type string    `json:"type"` // Literal["update"] | Literal["delete"]
}

type ResourceImage struct {
	Src    string `json:"src"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type ResourceLink struct {
	Src string `json:"src"`
}

type Resources struct {
	Article   *ResourceLink  `json:"article"`
	Icon      *ResourceImage `json:"icon"`
	Image     *ResourceImage `json:"image"`
	Log       *ResourceLink  `json:"log"`
	Props     *ResourceLink  `json:"props"`
	Thumbnail *ResourceImage `json:"thumbnail"`
}

type Bookmark struct {
	Authors       []string  `json:"authors"`
	Created       time.Time `json:"created"`
	Description   string    `json:"description"`
	DocumentType  string    `json:"document_type"`
	HasArticle    bool      `json:"has_article"`
	Href          string    `json:"href"`
	ID            string    `json:"id"`
	IsArchived    bool      `json:"is_archived"`
	IsDeleted     bool      `json:"is_deleted"`
	IsMarked      bool      `json:"is_marked"`
	Labels        []string  `json:"labels"`
	Lang          string    `json:"lang"`
	Loaded        bool      `json:"loaded"`
	ReadProgress  int       `json:"read_progress"`
	Resources     Resources `json:"resources"`
	Site          string    `json:"site"`
	SiteName      string    `json:"site_name"`
	State         int       `json:"state"`
	TextDirection string    `json:"text_direction"`
	Title         string    `json:"title"`
	Type          string    `json:"type"`
	Updated       time.Time `json:"updated"`
	URL           string    `json:"url"`
	WordCount     int       `json:"word_count"`
	Published     time.Time `json:"published"`
}

// IsVideo reports whether the bookmark is a video. Readeck's document
// `type` is one of "article", "photo" or "video" — the same discriminator
// behind its built-in "Videos" filter. Videos have no readable text for an
// e-reader, so both Kobo syncs exclude them.
func (b *Bookmark) IsVideo() bool {
	return b.Type == "video"
}

// Annotation is a Readeck highlight/annotation attached to a bookmark.
// Field set matches the live 0.23.2 wire format (see
// docs/research/readeck-annotations-api.md): selectors are body-relative
// XPath, offsets are rune offsets into the selected element's concatenated
// descendant text (end exclusive), and there is no `updated` field on
// annotations (only on bookmarks).
type Annotation struct {
	ID            string    `json:"id"`
	StartSelector string    `json:"start_selector"`
	StartOffset   int       `json:"start_offset"`
	EndSelector   string    `json:"end_selector"`
	EndOffset     int       `json:"end_offset"`
	Color         string    `json:"color"`
	Created       time.Time `json:"created"`
	Text          string    `json:"text"`
	Note          string    `json:"note"`
}

// AnnotationCreate is the POST /api/bookmarks/{id}/annotations body. Per the
// live API there is no `text` field (the server recomputes the highlighted
// text from its own article DOM and any client-supplied `text` is ignored),
// and `color` is required (≤32 chars).
type AnnotationCreate struct {
	StartSelector string `json:"start_selector"`
	StartOffset   int    `json:"start_offset"`
	EndSelector   string `json:"end_selector"`
	EndOffset     int    `json:"end_offset"`
	Color         string `json:"color"`
	Note          string `json:"note,omitempty"`
}

// AnnotationUpdate is the PATCH /api/bookmarks/{id}/annotations/{annotation_id}
// body. Only `color` (required) and `note` (optional) are bound; selectors,
// offsets and text are immutable and silently ignored.
type AnnotationUpdate struct {
	Color string `json:"color"`
	Note  string `json:"note,omitempty"`
}

// annotationUpdateResponse is the PATCH response shape: the full DOM-ordered
// annotation list plus an updated timestamp. The client only needs to know the
// call succeeded, so the payload is decoded for completeness but not exposed.
type annotationUpdateResponse struct {
	Annotations []Annotation `json:"annotations"`
	Updated     time.Time    `json:"updated"`
}
