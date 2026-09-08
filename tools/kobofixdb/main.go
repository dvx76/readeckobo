// Command kobofixdb generates a synthetic KoboReader.sqlite fixture at
// /tmp/kobofixture/KoboReader.sqlite for offline development and tests of the
// Readeck-on-Kobo highlight-sync agent.
//
// It documents (via seeded rows) the schema described in
// ../../docs/research/kobo-db-schema.md — which in turn is compiled from
// https://boehs.org/node/kobo-sqlite, https://samedwardes.com/blog/2024-06-23-querying-kobo-with-duckdb/,
// https://github.com/marcus-crane/october, https://github.com/voidberg/instakobo,
// https://github.com/kovidgoyal/calibre (devices/kobo/driver.py) and
// https://github.com/janlarres/kobo-utilities. See
// ../../docs/research/kobo-fixture-db.md for the row-by-row explanation.
//
// Notes:
//   - This directory is a *nested Go module* on purpose: it depends on
//     modernc.org/sqlite (pure-Go, CGO-free) without touching the repo's
//     root go.mod. When the sync agent is built (on-device and in tests), the
//     root go.mod should add the same dependency: go get modernc.org/sqlite@v1.46.0
//     (v1.46.0 is the newest release that keeps the repo's Go 1.24 baseline;
//     v1.48.0+ requires Go 1.25).
//
// Build & run (from this directory):
//
//	go run .                  # writes /tmp/kobofixture/KoboReader.sqlite (+ schema.sql)
//	OUT=/tmp/alt.sqlite go run .   # override output path
//
// Cross-compile check that the pure-Go driver works for the device:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o /tmp/kobofixdb .
//
// Inspect the result:
//
//	sqlite3 /tmp/kobofixture/KoboReader.sqlite ".tables"
//	sqlite3 /tmp/kobofixture/KoboReader.sqlite "SELECT BookTitle, VolumeID, ContentID, ContentType, MimeType, VolumeIndex FROM content ORDER BY BookTitle, VolumeIndex;"
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Output directory (override with env OUT for tests).
var outDir = envOr("OUT", "/tmp/kobofixture")

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Timestamps use Kobo's format: ISO8601 UTC, %Y-%m-%dT%H:%M:%SZ
// (calibre TIMESTAMP_STRING, https://github.com/kovidgoyal/calibre .../kobo/driver.py:1603).
const isoZ = "2006-01-02T15:04:05Z"

func nowZ() string { return time.Now().UTC().Format(isoZ) }

// Fixed IDs so the fixture is deterministic across runs.
const (
	// Book A — an imported Readeck article stored in the hidden .kobo area
	// (ContentType 6, kepub). VolumeID == book-row ContentID.
	articleAID     = "file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub"
	articleACh1    = articleAID + "!!OEBPS/xhtml/ch001.xhtml" // chapter ContentID uses the !! separator (october)
	articleACh2    = articleAID + "!!OEBPS/xhtml/ch002.xhtml"
	articleACh3    = articleAID + "!!OEBPS/xhtml/ch003.xhtml"
	articleABookID = "a2c0e9f4-0000-4000-8000-0000000000a1"

	// Book B — a "real" sideloaded kepub in the normal library path.
	realBookID     = "file:///mnt/onboard/books/Project Hail Mary - Andy Weir.kepub.epub"
	realBookCh1    = realBookID + "!!OEBPS/text/part0001.xhtml"
	realBookCh2    = realBookID + "!!OEBPS/text/part0002.xhtml"
	realBookBookID = "b3d1f0a5-1111-4111-8111-1111111111b2"

	// Bookmark UUIDs (Kobo uses a UUID per bookmark row).
	hlA1   = "9a3f7b2e-1111-4aaa-aaaa-000000000001"
	hlA2   = "9a3f7b2e-2222-4bbb-bbbb-000000000002"
	noteA1 = "9a3f7b2e-3333-4ccc-cccc-000000000003"
	hlB1   = "9a3f7b2e-4444-4ddd-dddd-000000000004"
)

func mustExec(db *sql.DB, q string, args ...any) {
	if _, err := db.Exec(q, args...); err != nil {
		panic(fmt.Sprintf("exec failed: %v\nSQL: %s", err, q))
	}
}

func main() {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}
	dbPath := filepath.Join(outDir, "KoboReader.sqlite")
	// Drop any stale fixture so tests always start clean.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	createSchema(db)
	seedContent(db)
	seedBookmarks(db)
	seedShelves(db)

	// Write a portable DDL artifact too (also satisfies the "sqlite3 CLI" fallback path).
	if err := os.WriteFile(filepath.Join(outDir, "schema.sql"), []byte(schemaDDL), 0o644); err != nil {
		panic(err)
	}

	fmt.Printf("fixture written: %s\n", dbPath)
	fmt.Printf("schema DDL:      %s\n", filepath.Join(outDir, "schema.sql"))
	fmt.Printf("tables: content(7 rows), Bookmark(4 rows), Shelf(1), ShelfContent(2), DbVersion(1)\n")
}

func createSchema(db *sql.DB) {
	for _, stmt := range splitStatements(schemaDDL) {
		if stmt != "" {
			mustExec(db, stmt)
		}
	}
}

// schemaDDL models the real tables (content columns follow the MobileRead
// "Backing up Pocket metadata" thread and october's Content struct; bools are
// stored as TEXT 'false'/'true' because the real device does that — see the
// DuckDB type-mismatch error in samedwardes.com).
//
// The DbVersion value (126) is a plausible 4.x number; the exact
// 4.38.23697 value is UNVERIFIED (see docs/research/kobo-db-schema.md §11).
const schemaDDL = `
CREATE TABLE DbVersion (
    version INTEGER NOT NULL
);

CREATE TABLE content (
    ContentID TEXT NOT NULL PRIMARY KEY,
    ContentType TEXT NOT NULL,
    MimeType TEXT NOT NULL,
    BookID TEXT,
    BookTitle TEXT,
    ImageId TEXT,
    Title TEXT COLLATE NOCASE,
    Attribution TEXT COLLATE NOCASE,
    Description TEXT,
    DateCreated TEXT,
    ShortCoverKey TEXT,
    adobe_location TEXT,
    Publisher TEXT,
    IsEncrypted BOOL,
    DateLastRead TEXT,
    FirstTimeReading BOOL,
    ChapterIDBookmarked TEXT,
    ParagraphBookmarked INTEGER,
    BookmarkWordOffset INTEGER,
    NumShortcovers INTEGER,
    VolumeIndex INTEGER,
    ___NumPages INTEGER,
    ReadStatus INTEGER,
    ___SyncTime TEXT,
    ___UserID TEXT NOT NULL,
    PublicationId TEXT,
    ___FileOffset INTEGER,
    ___FileSize INTEGER,
    ___PercentRead INTEGER,
    ___ExpirationStatus INTEGER,
    FavouritesIndex NOT NULL DEFAULT -1,
    Accessibility INTEGER DEFAULT 1,
    ContentURL TEXT,
    Language TEXT,
    BookshelfTags TEXT,
    IsDownloaded BIT NOT NULL DEFAULT 1,
    FeedbackType INTEGER DEFAULT 0,
    AverageRating INTEGER DEFAULT 0,
    Depth INTEGER,
    PageProgressDirection TEXT,
    InWishlist BOOL NOT NULL DEFAULT FALSE,
    ISBN TEXT,
    WishlistedDate TEXT DEFAULT '0000-00-00T00:00:00.000',
    FeedbackTypeSynced INTEGER DEFAULT 0,
    IsSocialEnabled BOOL NOT NULL DEFAULT TRUE,
    EpubType INT NOT NULL DEFAULT -1,
    Monetization INTEGER DEFAULT 2,
    ExternalId TEXT,
    Series TEXT,
    SeriesNumber TEXT,
    Subtitle TEXT,
    WordCount INTEGER DEFAULT -1,
    Fallback TEXT,
    RestOfBookEstimate INTEGER,
    CurrentChapterEstimate INTEGER,
    CurrentChapterProgress FLOAT,
    PocketStatus INTEGER DEFAULT 0,
    UnsyncedPocketChanges TEXT,
    ImageUrl TEXT,
    DateAdded TEXT,
    WorkId TEXT,
    Properties TEXT,
    RenditionSpread TEXT,
    RatingCount INTEGER DEFAULT 0,
    ReviewsSyncDate TEXT,
    MediaOverlay TEXT,
    MediaOverlayType TEXT,
    RedirectPreviewUrl TEXT,
    PreviewFileSize INTEGER,
    EntitlementId TEXT,
    CrossRevisionId TEXT,
    DownloadUrl TEXT,
    ReadStateSynced BIT NOT NULL DEFAULT false,
    TimesStartedReading INTEGER,
    TimeSpentReading INTEGER,
    LastTimeStartedReading TEXT,
    LastTimeFinishedReading TEXT,
    ApplicableSubscriptions TEXT,
    ExternalIds TEXT,
    PurchaseRevisionId TEXT,
    SeriesID TEXT,
    SeriesNumberFloat REAL,
    AdobeLoanExpiration TEXT,
    HideFromHomePage BIT,
    IsInternetArchive BOOL NOT NULL DEFAULT FALSE,
    titleKana TEXT,
    subtitleKana TEXT,
    seriesKana TEXT,
    attributionKana TEXT,
    publisherKana TEXT,
    IsPurchaseable BOOL DEFAULT TRUE,
    IsSupported BOOL DEFAULT TRUE,
    AnnotationsSyncToken TEXT,
    DateModified TEXT DEFAULT '0000-00-00T00:00:00.000',
    StorePages INTEGER DEFAULT 0,
    StoreWordCount INTEGER DEFAULT 0,
    StoreTimeToReadLowerEstimate INTEGER DEFAULT 0,
    StoreTimeToReadUpperEstimate INTEGER DEFAULT 0,
    Duration INTEGER DEFAULT 0,
    IsAbridged BOOL DEFAULT NULL,
    SyncConflictType INTEGER DEFAULT 0
);

CREATE TABLE Bookmark (
    BookmarkID TEXT NOT NULL PRIMARY KEY,
    VolumeID TEXT NOT NULL,
    ContentID TEXT NOT NULL,
    StartContainerPath TEXT NOT NULL,
    StartContainerChildIndex TEXT NOT NULL,
    StartOffset TEXT NOT NULL,
    EndContainerPath TEXT NOT NULL,
    EndContainerChildIndex TEXT NOT NULL,
    EndOffset TEXT NOT NULL,
    Text TEXT,
    Annotation TEXT,
    ExtraAnnotationData TEXT,
    DateCreated TEXT,
    ChapterProgress TEXT NOT NULL DEFAULT 0,
    Hidden TEXT NOT NULL DEFAULT 'false',
    Version TEXT,
    DateModified TEXT,
    Creator TEXT,
    UUID TEXT,
    UserID TEXT,
    SyncTime TEXT,
    Published TEXT NOT NULL DEFAULT 'false',
    ContextString TEXT,
    Type TEXT
);

CREATE TABLE Shelf (
    CreationDate TEXT NOT NULL,
    InternalName TEXT NOT NULL,
    LastModified TEXT NOT NULL,
    Name TEXT NOT NULL,
    _IsDeleted TEXT NOT NULL,
    _IsVisible TEXT NOT NULL,
    _IsSynced TEXT NOT NULL,
    Id TEXT,
    Type TEXT
);

CREATE TABLE ShelfContent (
    ShelfName TEXT NOT NULL,
    ContentId TEXT NOT NULL,
    DateModified TEXT NOT NULL,
    _IsDeleted TEXT NOT NULL,
    _IsSynced TEXT NOT NULL
);
`

func splitStatements(ddl string) []string {
	var out []string
	var cur []rune
	for _, r := range ddl {
		cur = append(cur, r)
		if r == ';' {
			out = append(out, string(cur))
			cur = nil
		}
	}
	return out
}

func seedContent(db *sql.DB) {
	// ---- Book A: Readeck article kepub (book row + 3 chapter rows) ----
	insertContentRow(db, articleAID, "6", "application/x-kobo-epub+zip", articleABookID,
		"Readeck Article One", "Readeck Article One", "Readeck", "2026-09-01T08:00:00Z",
		"2026-09-01T08:00:00Z", -1, 42, "true", 1240)
	insertContentRow(db, articleACh1, "6", "application/xhtml+xml", articleABookID,
		"Readeck Article One", "Chapter 1", "Readeck", "2026-09-01T08:00:00Z",
		"2026-09-01T08:00:00Z", 0, 42, "true", 400)
	insertContentRow(db, articleACh2, "6", "application/xhtml+xml", articleABookID,
		"Readeck Article One", "Chapter 2", "Readeck", "2026-09-01T08:00:00Z",
		"2026-09-01T08:00:00Z", 1, 42, "true", 420)
	insertContentRow(db, articleACh3, "6", "application/xhtml+xml", articleABookID,
		"Readeck Article One", "Chapter 3", "Readeck", "2026-09-01T08:00:00Z",
		"2026-09-01T08:00:00Z", 2, 42, "true", 420)

	// ---- Book B: real sideloaded kepub (book row + 2 chapter rows) ----
	insertContentRow(db, realBookID, "6", "application/x-kobo-epub+zip", realBookBookID,
		"Project Hail Mary", "Project Hail Mary", "Weir, Andy", "2026-08-15T12:30:00Z",
		"2026-08-15T12:30:00Z", -1, 15, "true", 118000)
	insertContentRow(db, realBookCh1, "6", "application/xhtml+xml", realBookBookID,
		"Project Hail Mary", "Chapter 1", "Weir, Andy", "2026-08-15T12:30:00Z",
		"2026-08-15T12:30:00Z", 0, 15, "true", 40000)
	insertContentRow(db, realBookCh2, "6", "application/xhtml+xml", realBookBookID,
		"Project Hail Mary", "Chapter 2", "Weir, Andy", "2026-08-15T12:30:00Z",
		"2026-08-15T12:30:00Z", 1, 15, "true", 40000)
}

// insertContentRow seeds one content row with the fields this project cares
// about; the rest take their column defaults.
func insertContentRow(db *sql.DB, contentID, contentType, mime, bookID, bookTitle, title,
	attribution, dateCreated, dateAdded string, volumeIndex, percentRead int, isDownloaded string,
	wordCount int) {
	mustExec(db, `INSERT INTO content
		(ContentID, ContentType, MimeType, BookID, BookTitle, Title, Attribution,
		 DateCreated, DateAdded, VolumeIndex, ___PercentRead, IsDownloaded, WordCount,
		 ReadStatus, FirstTimeReading, ___UserID)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,'false','')`,
		contentID, contentType, mime, bookID, bookTitle, title, attribution,
		dateCreated, dateAdded, volumeIndex, percentRead, isDownloaded, wordCount)
}

func seedBookmarks(db *sql.DB) {
	// Highlight 1 — single span in ch1 of Article A.
	// StartContainerPath/EndContainerPath are CSS selectors with escaped dots,
	// matching boehs.org (`span#kobo\.5\.2`) and kepubify's
	// <span class="koboSpan" id="kobo.5.2">.
	insertBookmark(db, hlA1, articleAID, articleACh1,
		`span#kobo\.5\.2`, "-99", "0",
		`span#kobo\.5\.2`, "-99", "58",
		"Readeck lets you keep highlights of articles you read on your e-reader.",
		"", "2026-09-02T09:15:00Z", "0.25", "false")

	// Highlight 2 — spans two paragraphs (different ids), ch2 of Article A.
	insertBookmark(db, hlA2, articleAID, articleACh2,
		`span#kobo\.3\.1`, "-99", "12",
		`span#kobo\.3\.2`, "-99", "40",
		"Offsets are character positions within the start and end spans.",
		"", "2026-09-02T10:00:00Z", "0.50", "false")

	// Note — an annotation attached to a highlighted passage (Type='note',
	// Text = highlighted text, Annotation = the note).
	insertBookmark(db, noteA1, articleAID, articleACh1,
		`span#kobo\.9\.4`, "-99", "0",
		`span#kobo\.9\.4`, "-99", "30",
		"Sync this back to Readeck later.",
		"Need to double-check this claim before publishing.",
		"2026-09-03T11:00:00Z", "0.25", "false")

	// Highlight 3 — Book B, to exercise per-volume filtering.
	insertBookmark(db, hlB1, realBookID, realBookCh1,
		`span#kobo\.12\.1`, "-99", "0",
		`span#kobo\.12\.2`, "-99", "90",
		"Rocky had a problem. Actually, he had a lot of problems.",
		"", "2026-08-20T18:45:00Z", "0.02", "false")
}

func insertBookmark(db *sql.DB, id, volumeID, contentID, startPath, startChild, startOff,
	endPath, endChild, endOff, text, annotation, dateCreated, chapterProgress, hidden string) {
	mustExec(db, `INSERT INTO Bookmark
		(BookmarkID, VolumeID, ContentID, StartContainerPath, StartContainerChildIndex,
		 StartOffset, EndContainerPath, EndContainerChildIndex, EndOffset, Text, Annotation,
		 ExtraAnnotationData, DateCreated, ChapterProgress, Hidden, Version, DateModified,
		 Creator, UUID, UserID, SyncTime, Published, ContextString, Type)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,NULL,?,NULL,NULL,'',NULL,'false',NULL,?)`,
		id, volumeID, contentID, startPath, startChild, startOff,
		endPath, endChild, endOff, text, annotation,
		dateCreated, chapterProgress, hidden, dateModified(id), bookmarkType(id))
}

func bookmarkType(id string) string {
	if id == noteA1 {
		return "note"
	}
	return "highlight"
}

func dateModified(id string) string {
	// Later than DateCreated, like a real edited annotation.
	switch id {
	case hlA1:
		return "2026-09-02T09:16:00Z"
	case hlA2:
		return "2026-09-02T10:01:00Z"
	case noteA1:
		return "2026-09-03T11:05:00Z"
	default:
		return "2026-08-20T18:46:00Z"
	}
}

func seedShelves(db *sql.DB) {
	// "Readeck" collection. Modern firmware (DbVersion >= 64) uses the
	// Id + Type columns; calibre's driver writes Type='UserTag'
	// (calibre .../kobo/driver.py:3686-3693). Bools are text 'false'/'true'.
	ts := nowZ()
	mustExec(db, `INSERT INTO Shelf
		(CreationDate, InternalName, LastModified, Name, _IsDeleted, _IsVisible, _IsSynced, Id, Type)
		VALUES (?,?,?,?, 'false', 'true', 'false', ?, 'UserTag')`,
		ts, "Readeck", ts, "Readeck", "Readeck")

	// Put both imported books into the Readeck collection. ShelfContent
	// references the *book row* ContentID (= VolumeID) of each kepub.
	mustExec(db, `INSERT INTO ShelfContent
		(ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
		VALUES (?,?,?, 'false', 'false')`, "Readeck", articleAID, ts)
	mustExec(db, `INSERT INTO ShelfContent
		(ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
		VALUES (?,?,?, 'false', 'false')`, "Readeck", realBookID, ts)

	// DbVersion row (value unverified for 4.38; see docs §11).
	mustExec(db, `INSERT INTO DbVersion (version) VALUES (126)`)
}
