package main

import (
	"context"
	"database/sql"
	"time"
)

// This file implements the *write* side of the device DB maintenance, with
// the exact SQL patterns from docs/research/kobo-db-schema.md §6 (calibre's
// KoboTouch driver) as corrected by the Kobo Utilities findings:
//
//   - content.___SyncTime drives the "Date added"/Recent sort on modern
//     firmware (Libra 2, 4.38.x): "Date added" uses ___SyncTime, "Recent"
//     uses MAX(___SyncTime, DateLastRead) (davidfor, MobileRead t=347000).
//     Kobo Utilities' "Update metadata → Date added" writes ___SyncTime,
//     while content.DateCreated is the publishing date shown on the book
//     details screen (calibre maps pubdate → DateCreated).
//   - To satisfy both old firmware (which sorted collections by
//     ShelfContent.DateModified) and new firmware (which sorts by the book's
//     ___SyncTime), the agent sets all three to the Readeck "date added"
//     (bookmark.created): content.___SyncTime, content.DateCreated (so the
//     details screen agrees) and ShelfContent.DateModified on insert.
//   - Shelf + ShelfContent membership uses the calibre upsert pattern; the
//     DbVersion >= 64 shelf shape carries Id + Type columns.
//
// The agent NEVER inserts content rows: Nickel owns content; we only UPDATE
// date columns on rows whose ContentID we positively know is one of our
// imports. Because bookmark.created is immutable (unlike updated, which moves
// on every edit/re-fetch), the ensure is idempotent and self-healing: every
// pass converges already-imported rows to the correct timestamp instead of
// stamping only once.

// setDateCreated updates the DateCreated of exactly one book row (guarded to
// book rows: VolumeIndex -1 or unset). ts must already be formatted as
// %Y-%m-%dT%H:%M:%SZ. Prefer setBookAddedDate, which sets both date columns
// that drive sorting.
func setDateCreated(ctx context.Context, db *sql.DB, contentID, ts string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE content SET DateCreated = ?
		 WHERE ContentID = ? AND (VolumeIndex = -1 OR VolumeIndex IS NULL)`,
		ts, contentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// setBookAddedDate updates both date columns that determine the Kobo sort
// order for exactly one book row (guarded to book rows): ___SyncTime (the
// actual "Date added"/Recent sort key on modern firmware) and DateCreated
// (publishing-date display; kept in sync so old firmware and the details
// screen agree). ts must already be formatted as %Y-%m-%dT%H:%M:%SZ.
func setBookAddedDate(ctx context.Context, db *sql.DB, contentID, ts string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE content SET DateCreated = ?, ___SyncTime = ?
		 WHERE ContentID = ? AND (VolumeIndex = -1 OR VolumeIndex IS NULL)`,
		ts, ts, contentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ensureShelf creates the named shelf when it is missing (calibre
// check_for_bookshelf). The modern firmware shape (DbVersion >= 64) adds
// Id/Type columns; the legacy shape omits them. Re-adding an existing
// deleted shelf flips _IsDeleted back to 'false'.
func ensureShelf(ctx context.Context, db *sql.DB, name string, now time.Time, version int) error {
	ts := formatKoboTime(now)
	var deleted string
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(_IsDeleted, 'false') FROM Shelf WHERE Name = ? LIMIT 1", name).Scan(&deleted)
	switch {
	case err == sql.ErrNoRows:
		if version >= 64 {
			// Note: do not reuse `err` here — `id, err := newUUIDv4()`
			// would shadow the outer err and the insert below would assign
			// the shadow, making `return err` report ErrNoRows even after a
			// successful insert.
			id, uerr := newUUIDv4()
			if uerr != nil {
				return uerr
			}
			_, err = db.ExecContext(ctx,
				`INSERT INTO Shelf
				   (CreationDate, InternalName, LastModified, Name, _IsDeleted, _IsVisible, _IsSynced, Id, Type)
				 VALUES (?, ?, ?, ?, 'false', 'true', 'false', ?, 'UserTag')`,
				ts, name, ts, name, id)
		} else {
			_, err = db.ExecContext(ctx,
				`INSERT INTO Shelf
				   (CreationDate, InternalName, LastModified, Name, _IsDeleted, _IsVisible, _IsSynced)
				 VALUES (?, ?, ?, ?, 'false', 'true', 'false')`,
				ts, name, ts, name)
		}
		return err
	case err != nil:
		return err
	default:
		if deleted != "false" {
			_, err = db.ExecContext(ctx,
				"UPDATE Shelf SET _IsDeleted = 'false' WHERE Name = ?", name)
		}
		return err
	}
}

// addContentToShelf records one book row's membership in a shelf, following
// calibre's set_bookshelf upsert: insert when missing, otherwise revive
// (_IsDeleted 'true' → 'false'). dateModified must already be formatted as
// %Y-%m-%dT%H:%M:%SZ and is the Readeck "date added" (bookmark.created), not
// the sync time: old firmware sorted collections by this column, so writing
// the sync time (as calibre does) would lose the Readeck order for bulk
// imports. Existing rows whose DateModified differs are converged to the
// desired value so devices synced before this fix self-heal.
func addContentToShelf(ctx context.Context, db *sql.DB, shelf, contentID string, dateModified string) error {
	var deleted, current string
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(_IsDeleted, 'false'), COALESCE(DateModified, '') FROM ShelfContent WHERE ShelfName = ? AND ContentId = ? LIMIT 1",
		shelf, contentID).Scan(&deleted, &current)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.ExecContext(ctx,
			`INSERT INTO ShelfContent (ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
			 VALUES (?, ?, ?, 'false', 'false')`,
			shelf, contentID, dateModified)
		return err
	case err != nil:
		return err
	default:
		if deleted != "false" {
			_, err = db.ExecContext(ctx,
				`UPDATE ShelfContent
				 SET _IsDeleted = 'false', _IsSynced = 'false', DateModified = ?
				 WHERE ShelfName = ? AND ContentId = ?`,
				dateModified, shelf, contentID)
			return err
		}
		if current != dateModified {
			_, err = db.ExecContext(ctx,
				`UPDATE ShelfContent
				 SET _IsSynced = 'false', DateModified = ?
				 WHERE ShelfName = ? AND ContentId = ?`,
				dateModified, shelf, contentID)
		}
		return err
	}
}
