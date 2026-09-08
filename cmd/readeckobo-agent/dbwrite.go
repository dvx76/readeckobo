package main

import (
	"context"
	"database/sql"
	"time"
)

// This file implements the *write* side of the device DB maintenance, with
// the exact SQL patterns from docs/research/kobo-db-schema.md §6 (calibre's
// KoboTouch driver):
//
//   - content.DateCreated drives the "Date added"/Recent sort — set it (once,
//     only for rows Nickel created from *our* imports) to the server's
//     article.updated timestamp.
//   - Shelf + ShelfContent membership uses the calibre upsert pattern; the
//     DbVersion >= 64 shelf shape carries Id + Type columns.
//
// The agent NEVER inserts content rows: Nickel owns content; we only UPDATE
// DateCreated on rows whose ContentID we positively know is one of our
// imports, and only when we just observed the row appear (index state
// "downloaded", see agent.go).

// setDateCreated updates the DateCreated of exactly one book row (guarded to
// book rows: VolumeIndex -1 or unset). ts must already be formatted as
// %Y-%m-%dT%H:%M:%SZ.
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
// (_IsDeleted 'true' → 'false').
func addContentToShelf(ctx context.Context, db *sql.DB, shelf, contentID string, now time.Time) error {
	ts := formatKoboTime(now)
	var deleted string
	err := db.QueryRowContext(ctx,
		"SELECT COALESCE(_IsDeleted, 'false') FROM ShelfContent WHERE ShelfName = ? AND ContentId = ? LIMIT 1",
		shelf, contentID).Scan(&deleted)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.ExecContext(ctx,
			`INSERT INTO ShelfContent (ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
			 VALUES (?, ?, ?, 'false', 'false')`,
			shelf, contentID, ts)
		return err
	case err != nil:
		return err
	default:
		if deleted != "false" {
			_, err = db.ExecContext(ctx,
				`UPDATE ShelfContent
				 SET _IsDeleted = 'false', _IsSynced = 'false', DateModified = ?
				 WHERE ShelfName = ? AND ContentId = ?`,
				ts, shelf, contentID)
		}
		return err
	}
}
