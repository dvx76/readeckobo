package main

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Highlights live in the Bookmark table; the canonical SQL shape is
// documented in docs/research/kobo-db-schema.md §9.1. We select every
// non-hidden highlight/note row whose VolumeID sits under the agent's
// directory and do the final "is this one of MY kepubs" filter in Go (see
// collectUploadItems). The LIKE pattern is a constant without wildcard
// characters, so it is safe unescaped.
//
// DateCreated/DateModified are ISO8601 UTC with a Z suffix on the device;
// some rows carry NULL or odd values — they come back as NullStrings and are
// normalized by the caller.

// deviceHighlight is one Bookmark row (raw device values).
type deviceHighlight struct {
	RowID              string // BookmarkID (Kobo UUID) = the wire bookmark_row_id
	VolumeID           string
	Type               string // highlight | note
	Text               string
	Annotation         sql.NullString
	StartContainerPath sql.NullString
	StartOffset        sql.NullString
	EndContainerPath   sql.NullString
	EndOffset          sql.NullString
	DateCreated        sql.NullString
	DateModified       sql.NullString
}

const highlightScanSQL = `
SELECT b.BookmarkID, b.VolumeID, b.Type,
       b.Text, b.Annotation,
       b.StartContainerPath, b.StartOffset, b.EndContainerPath, b.EndOffset,
       b.DateCreated, b.DateModified
FROM Bookmark AS b
WHERE b.VolumeID LIKE '%/.kobo/readeck/%'
  AND b.Type IN ('highlight','note')
  AND b.Hidden != 'true'
  AND b.Text IS NOT NULL AND b.Text != ''
ORDER BY b.DateCreated, b.BookmarkID`

// scanHighlights runs the canonical extraction over an already-open snapshot
// connection. Volume filtering happens afterwards (filterManaged), because
// the row's VolumeID must be matched against the index's file names.
func scanHighlights(ctx context.Context, db *sql.DB) ([]deviceHighlight, error) {
	rows, err := db.QueryContext(ctx, highlightScanSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deviceHighlight
	for rows.Next() {
		var h deviceHighlight
		if err := rows.Scan(
			&h.RowID, &h.VolumeID, &h.Type,
			&h.Text, &h.Annotation,
			&h.StartContainerPath, &h.StartOffset, &h.EndContainerPath, &h.EndOffset,
			&h.DateCreated, &h.DateModified,
		); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// managedFilename reports whether the row's volume maps to a kepub we still
// manage (downloaded or imported, i.e. not tombstoned by a server removal).
func managedFilename(volumeID string, idx *indexStore) (string, bool) {
	name := filenameOfVolumeID(volumeID)
	e, ok := idx.get(name)
	if !ok || e.State == stateRemoved {
		return "", false
	}
	return name, true
}

// effectiveModified returns the value used for change detection: DateModified
// when present, else DateCreated, else "" (a row with neither cannot be
// change-tracked and is skipped by the uploader).
func effectiveModified(h deviceHighlight) string {
	if h.DateModified.Valid && strings.TrimSpace(h.DateModified.String) != "" {
		return h.DateModified.String
	}
	if h.DateCreated.Valid && strings.TrimSpace(h.DateCreated.String) != "" {
		return h.DateCreated.String
	}
	return ""
}

func nullString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

// parseOffset converts a device offset column to the integer the wire
// contract uses. Device rows store offsets as text (fixture) or integers
// (real rows, see boehs.org) — both scan into the string columns above.
func parseOffset(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return v, true
}

// buildUploadItem converts a device row into a wire payload item. It returns
// ok=false when the row cannot be represented (unparseable offsets) so the
// caller can skip it with a logged reason instead of poisoning the batch.
func buildUploadItem(h deviceHighlight, bookmarkID, effective string) (uploadItem, bool) {
	startOff, sok := parseOffset(nullString(h.StartOffset))
	endOff, eok := parseOffset(nullString(h.EndOffset))
	if !sok || !eok {
		return uploadItem{}, false
	}
	item := uploadItem{
		BookmarkID:    bookmarkID,
		BookmarkRowID: h.RowID,
		StartPath:     nullString(h.StartContainerPath),
		StartOffset:   startOff,
		EndPath:       nullString(h.EndContainerPath),
		EndOffset:     endOff,
		Text:          h.Text,
		Annotation:    nullString(h.Annotation),
		Type:          h.Type,
		DateCreated:   nullString(h.DateCreated),
		DateModified:  effective,
	}
	return item, true
}
