package readeck

import (
	"context"
	"time"
)

// ClientInterface defines the interface for the Readeck API client.
type ClientInterface interface {
	GetBookmarksSync(ctx context.Context, since *time.Time) ([]BookmarkSync, error)
	GetBookmarks(ctx context.Context, site string, isArchived *bool) ([]Bookmark, error)
	GetBookmarkDetails(ctx context.Context, id string) (*Bookmark, error)
	SyncBookmarksContent(ctx context.Context, ids []string) (map[string]*Bookmark, error)
	GetBookmarkArticle(ctx context.Context, id string) (string, error)
	GetBookmarkAnnotations(ctx context.Context, id string) ([]Annotation, error)
	CreateAnnotation(ctx context.Context, id string, body AnnotationCreate) (*Annotation, error)
	UpdateAnnotation(ctx context.Context, id, annotationID string, body AnnotationUpdate) error
	UpdateBookmark(ctx context.Context, id string, updates map[string]any) error
	CreateBookmark(ctx context.Context, bookmarkURL string) error
}
