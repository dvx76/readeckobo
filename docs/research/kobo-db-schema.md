# Kobo Device Side — KoboReader.sqlite Schema & On-Device Sync Research

**Stream:** B (highlight-sync project) · **Date:** 2026-09-08
**Target device:** Kobo Libra 2 (Mark 7, armv7l), firmware **4.38.23697** (Nickel 4.x, not 5.x).
**Purpose:** document everything needed to build a wireless on-device agent that (1) imports
Readeck articles as kepubs into the stock reader, (2) puts them in a dedicated collection sorted
by "Date added", and (3) reads highlights/notes out of `KoboReader.sqlite` for upload.

> **Status:** we do **not** yet have a real-device DB dump. Everything below is compiled from
> community sources and upstream code. Anything marked ⚠️ UNVERIFIED must be checked against the
> real device (see [Open questions](#11-open-questions-to-verify-on-the-real-device-later)).

**Primary sources** (all read as of 2026-09-08):

| Source | URL |
|---|---|
| Evan Boehs "Kobo SQLite" | https://boehs.org/node/kobo-sqlite |
| Sam Edwardes "Querying Kobo with DuckDB" | https://samedwardes.com/blog/2024-06-23-querying-kobo-with-duckdb/ |
| october (Go, reads Kobo DB for Readwise) | https://github.com/marcus-crane/october (`v2/pkg/kobo/*.go`) |
| instakobo (Deno, WAL-safe reads) | https://github.com/voidberg/instakobo (`src/kobo.ts`) |
| kobo-highlights (Go, SQL joins) | https://github.com/ozmodiar/kobo-highlights (`main.go`) |
| MobileRead "Backing up Pocket metadata" | https://www.mobileread.com/forums/showthread.php?t=368317 |
| MobileRead "Sorting via recent (date added) doesn't work" | https://www.mobileread.com/forums/showthread.php?t=333040 |
| Calibre KoboTouch driver | https://github.com/kovidgoyal/calibre (`src/calibre/devices/kobo/{driver,db,books}.py`) |
| Kobo Utilities (KoboTouch plugin, davidfor/janlarres) | https://github.com/janlarres/kobo-utilities |
| kepubify | https://github.com/pgaskin/kepubify (`kepub/transform.go`) |
| NickelDBus | https://github.com/shermp/NickelDBus (+ https://shermp.github.io/NickelDBus/) |
| NickelMenu | https://github.com/pgaskin/NickelMenu (+ https://pgaskin.net/NickelMenu/) |
| KoSync (Go on-device binary) | https://github.com/gshzn/kosync |
| KoboCloud (udev/network hooks) | https://github.com/fsantini/KoboCloud |
| Kobo Labs EPUB spec (sideloading) | https://github.com/kobolabs/epub-spec (`README.md`) |
| Kobo root telnet guide | https://yingtongli.me/blog/2018/07/30/kobo-telnet.html |
| MobileRead Kobo hacking wiki | https://wiki.mobileread.com/wiki/Kobo_Touch_Hacking |
| modernc.org/sqlite (pure-Go driver) | https://pkg.go.dev/modernc.org/sqlite |

---

## 1. Database overview

* Location: **`.kobo/KoboReader.sqlite`** at the root of onboard storage (mount point
  `/mnt/onboard` on-device, e.g. `KOBOeReader/.kobo/KoboReader.sqlite` over USB).
  Source: boehs.org, samedwardes.com, instakobo `src/kobo.ts:18-20`.
* The DB is **SQLite in WAL mode** on the device; `KoboReader.sqlite-wal` / `-shm` siblings exist
  while Nickel has it open. Kobo checkpoints the WAL into the main file when the volume is
  ejected/unmounted (instakobo `src/kobo.ts:22-31`, Calibre `db.py:28-46`). **This drives the
  WAL-safe read recipe — see §9.**
* Column types are **inconsistent** (bools stored as text `'true'/'false'` in some tables/rows,
  integers elsewhere) — DuckDB must set `SET GLOBAL sqlite_all_varchar = true` to read it
  (samedwardes.com).
* A `DbVersion` table holds a schema version; Calibre reads `SELECT version FROM dbversion`
  (`db.py:59`) and uses it to decide which columns exist (e.g. `Shelf.Type` needs
  `dbversion >= 64` — `driver.py:3687-3693`). ⚠️ Actual numeric value for 4.38.23697 UNVERIFIED.

Table list (33 tables, `boehs.org` and samedwardes.com agree):

```
AbTest  Achievement  Activity  AnalyticsEvents  Authors  BookAuthors  Bookmark
DbVersion  DropboxItem  Event  GDriveItem  KoboPlusAssetGroup  KoboPlusAssets
OverDriveCards  OverDriveCheckoutBook  OverDriveLibrary  Reviews  Rules  Shelf
ShelfContent  SubscriptionProducts  SyncQueue  Tab  Wishlist  WordList  content
content_keys  content_settings  ratings  shortcover_page  user  volume_shortcovers
volume_tabs
```

The three tables that matter for this project: **`content`**, **`Bookmark`**, **`Shelf`**/**`ShelfContent`**.

---

## 2. `content` table — columns of interest

The `content` table has **one row per chapter** for every book (a "book row" with
`VolumeIndex = -1` plus per-chapter rows with `VolumeIndex >= 0`). See
samedwardes.com ("List all books and chapters": same `BookTitle` repeated with `Title` =
chapter name and `VolumeIndex` 0..n) and october `content.go:106-115` which counts books with
`ContentType = 6 AND VolumeIndex = -1 AND MimeType = 'application/x-kobo-epub+zip'`.

Full column list (verbatim, matches MobileRead "Backing up Pocket metadata" post
and october `v2/pkg/kobo/content.go:3-104`; full typed DDL in the Kobo Utilities test fixture
`tests/kobo-schema.sql`):

```
ContentID, ContentType, MimeType, BookID, BookTitle, ImageId, Title, Attribution, Description,
DateCreated, ShortCoverKey, adobe_location, Publisher, IsEncrypted, DateLastRead,
FirstTimeReading, ChapterIDBookmarked, ParagraphBookmarked, BookmarkWordOffset, NumShortcovers,
VolumeIndex, ___NumPages, ReadStatus, ___SyncTime, ___UserID, PublicationId, ___FileOffset,
___FileSize, ___PercentRead, ___ExpirationStatus, FavouritesIndex, Accessibility, ContentURL,
Language, BookshelfTags, IsDownloaded, FeedbackType, AverageRating, Depth, PageProgressDirection,
InWishlist, ISBN, WishlistedDate, FeedbackTypeSynced, IsSocialEnabled, EpubType, Monetization,
ExternalId, Series, SeriesNumber, Subtitle, WordCount, Fallback, RestOfBookEstimate,
CurrentChapterEstimate, CurrentChapterProgress, PocketStatus, UnsyncedPocketChanges, ImageUrl,
DateAdded, WorkId, Properties, RenditionSpread, RatingCount, ReviewsSyncDate, MediaOverlay,
MediaOverlayType, RedirectPreviewUrl, PreviewFileSize, EntitlementId, CrossRevisionId,
DownloadUrl, ReadStateSynced, TimesStartedReading, TimeSpentReading, LastTimeStartedReading,
LastTimeFinishedReading, ApplicableSubscriptions, ExternalIds, PurchaseRevisionId, SeriesID,
SeriesNumberFloat, AdobeLoanExpiration, HideFromHomePage, IsInternetArchive, titleKana,
subtitleKana, seriesKana, attributionKana, publisherKana, IsPurchaseable, IsSupported,
AnnotationsSyncToken, DateModified, StorePages, StoreWordCount, StoreTimeToReadLowerEstimate,
StoreTimeToReadUpperEstimate, Duration, IsAbridged, SyncConflictType
```

Columns of interest for us:

| Column | Type (fixture) | Meaning / notes |
|---|---|---|
| `ContentID` | TEXT PK | Unique id of the row. **Book row:** `file:///mnt/onboard/<path>.kepub.epub`. **Chapter row:** `file:///mnt/onboard/<path>.kepub.epub!!<chpath>` (kepub, `!!` separator — see §5). Store books use a GUID; calibre's driver distinguishes sideloaded vs purchased via `contentID.startswith('file')` (`books.py:91-101`). |
| `ContentType` | TEXT | 6 = epub/kepub ("book" incl. store + sideloaded kepub); 16 = pdf; 901 = txt/html; 999 = old html; 6 also used for Pocket rows. Sources: calibre `driver.py:724-752`; MobileRead pocket thread. ⚠️ exact enum UNVERIFIED beyond these. |
| `MimeType` | TEXT | Book row: `application/x-kobo-epub+zip`. Chapter row: `application/xhtml+xml`. Pocket: `application/x-kobo-html+pocket`. (october `content.go:106-115`, MobileRead thread.) |
| `BookTitle` | TEXT | Book title (repeat on every chapter row). |
| `Title` | TEXT | Chapter title on chapter rows; same as BookTitle on book rows. `COLLATE NOCASE` per fixture. |
| `Attribution` | TEXT | Author, e.g. `Lastname, Firstname` (`COLLATE NOCASE`). |
| `DateCreated` | TEXT | **Import/pubdate timestamp** `%Y-%m-%dT%H:%M:%SZ` (Calibre `TIMESTAMP_STRING`, `driver.py:1603`). Drives the device's "Date added"/"Recent" sort — see §7. |
| `DateAdded` | TEXT | Used by Pocket/store rows; **not** what the "Date added" sort reads for sideloads (see §7). MobileRead pocket query selects it. ⚠️ interplay with DateCreated UNVERIFIED on-device. |
| `DateLastRead` | TEXT | ISO timestamp of last read; instakobo parses it for progress timestamps (`kobo.ts:96-98`). |
| `___PercentRead` | INTEGER | 0–100 reading progress (instakobo `kobo.ts:81`, `105`). |
| `IsDownloaded` | TEXT/'true' | `'true'`/`'false'` as text; pocket query filters `isDownloaded = 'true'` (MobileRead thread). |
| `VolumeIndex` | INTEGER | `-1` for the book row, `0..n` for chapters (october `content.go:106-115`). |
| `BookID` | TEXT | Groups chapters of the same publication. |
| `ContentURL` | TEXT | Original URL for Pocket rows (MobileRead thread: "ContentURL appears to be the original URL"). |
| `WordCount`, `Series`, `SeriesNumber`, `Subtitle`, `Language`, `Publisher`, `ISBN`… | | Standard metadata. |
| `ReadStatus`, `FirstTimeReading`, `ChapterIDBookmarked`, `ParagraphBookmarked` | | Reading position/resume state. |

Real Pocket row query from the MobileRead thread (faceless007, post #6):

```sql
SELECT ContentID, ContentURL, Title, Description, DateAdded, WordCount
  FROM content
 WHERE MimeType='application/x-kobo-html+pocket'
   AND ContentType = 6
   AND isDownloaded = 'true';
```

> Note `ContentID` for Pocket articles is the *folder name* under `.kobo/articles` (PeterT, post
> #5: "ContentID is the name of the folder within .kobo/articles").

---

## 3. `Bookmark` table — columns and semantics

Verbatim column list + descriptions from boehs.org; full 24-column layout confirmed by
samedwardes.com's `DESCRIBE Bookmark` and october `v2/pkg/kobo/bookmark.go:3-29`:

| Column | Example (boehs.org) | Notes |
|---|---|---|
| `BookmarkID` | `7ab2b29e-0bda-4d26-b48f-792e131dd67a` | UUID, PK. |
| `VolumeID` | store: GUID; sideloaded: `file:///mnt/onboard/...` | Which book. |
| `ContentID` | store: `<guid>!OEBPS!xhtml/07_Epigraph.xhtml`; sideloaded: `file:///mnt/onboard/<file>.kepub.epub!!OEBPS/xhtml/....xhtml` (or same as VolumeID when the book has no chapters) | Which chapter. Join target for chapter titles. |
| `StartContainerPath` | kepub: `span#kobo\.5\.2`; epub: `OEBPS/Text/chapter1.xhtml#point(/1/4/2/9:1)` | CSS selector of the start span. See §5 for kepub ids. |
| `StartContainerChildIndex` | `-99` | Always `-99` in boehs' data. |
| `StartOffset` | `0` | Character offset within the start span element (relative to the element; boehs: "probably offset relative to the selected element"). |
| `EndContainerPath` | `span#kobo\.2\.1` | CSS selector of the end span. |
| `EndContainerChildIndex` | `-99` | Always `-99`. |
| `EndOffset` | `52` | Positive character offset within the end span. |
| `Text` | `Do not go where the path may lead.\nGo instead...` | Full highlighted text (may contain newlines). |
| `Annotation` | NULL or `''` | The user's note, if any. |
| `ExtraAnnotationData` | NULL | Unknown; NULL for boehs. |
| `DateCreated` | `2020-03-30T14:39:02Z` | ISO8601 **UTC** (`Z`). ⚠️ Not guaranteed on every row — see pitfalls §10. |
| `ChapterProgress` | `1.0` / `0.712765957446809` | Float 0–1 progress of this highlight within the book (samedwardes data shows long floats). |
| `Hidden` | `false` | `true` when the annotation is slated for deletion/sync-back (boehs hypothesis; ⚠️ UNVERIFIED). **Filter `Hidden != 'true'` when reading.** |
| `Version` | `0`/`NULL` | `NULL` for non-synced (boehs). |
| `DateModified` | `2020-04-08T17:31:49Z` | ISO8601. |
| `Creator`, `UUID` | NULL | Always NULL in boehs' data. |
| `UserID` | `d10d6022-b808-4c91-a4f5-15f30e0551f0` | User GUID. |
| `SyncTime` | `2020-04-08T17:31:49Z` | NULL until synced. |
| `Published` | `false` | Always false in boehs' data. |
| `ContextString` | — | Only set when `Type = dogear`. |
| `Type` | `highlight` \| `note` \| `dogear` | Row kind. **Read `'highlight'` and `'note'`; ignore `dogear`** (page-turn marks). |

Example real row (verbatim from boehs.org; a **store-bought kepub**):

```
7ab2b29e-0bda-4d26-b48f-792e131dd67a
|31c732b6-e4ff-4932-a6e8-3cef46cb513e                          -- VolumeID (store GUID)
|31c732b6-e4ff-4932-a6e8-3cef46cb513e!OEBPS!xhtml/07_Epigraph.xhtml  -- ContentID (chapter)
|span#kobo\.1\.1                                                  -- StartContainerPath
|-99|0                                                            -- StartContainerChildIndex, StartOffset
|span#kobo\.2\.1                                                  -- EndContainerPath
|-99|52                                                           -- EndContainerChildIndex, EndOffset
|Do not go where the path may lead.
 Go instead where there is no path and leave a trail.              -- Text
|(empty)|(empty)                                                   -- Annotation, ExtraAnnotationData
|1.0                                                              -- ChapterProgress
|false|2518167216574580235|2020-03-30T14:39:02Z                   -- Hidden, Version, DateCreated
|(empty)|(empty)|d10d6022-b808-4c91-a4f5-15f30e0551f0            -- Creator, UUID, UserID
|2020-04-08T17:31:49Z|false|(empty)|highlight                     -- SyncTime, Published, ContextString, Type
```

**Offset semantics (best current understanding):** for kepubs the container is the
`<span id="kobo.N.M">`; `StartOffset`/`EndOffset` are character offsets *within the start/end
span text*, so a single-span highlight looks like
`StartContainerPath=span#kobo\.5\.2, StartOffset=12, EndContainerPath=span#kobo\.5\.2, EndOffset=58`
and a multi-span highlight spans two different `kobo.N.M` ids. ⚠️ Exact character-counting rules
(Unicode code points vs UTF-16 units, whitespace handling) UNVERIFIED — see §11.

---

## 4. Sideloaded kepub row shapes

Two content-format observations that matter when writing our own rows or matching the device:

1. **`VolumeID`** for a sideloaded book is `file:///mnt/onboard/<relative/path>.kepub.epub`
   (boehs.org; october `bookmark.go:42-47` selects `VolumeID LIKE '%file:///%'` to find
   sideloaded books). Store books use a bare GUID.
2. **`ContentID` for chapters** of a sideloaded kepub uses a `!!` separator:
   `file:///mnt/onboard/books/Title.kepub.epub!!OEBPS/xhtml/ch001.xhtml`
   (october `v2/pkg/kobo/utils.go:30-40`: "`.kepub.epub => !!`", `strings.Split(path, ".kepub.epub!!")[1]`).
   Plain (non-kepub) sideloaded epubs use `#`: `...epub#OEBPS/Text/chapter.xhtml`.
   Boehs.org writes the sideloaded separator as `!ops!`; **the `!!` form from October is the
   better-corroborated one — flag as UNVERIFIED, see §11.**
3. **Book row vs chapter rows:** the book row (`VolumeIndex=-1`, `MimeType='application/x-kobo-epub+zip'`)
   is the row whose `ContentID` = `VolumeID`; chapter rows have `MimeType='application/xhtml+xml'`.
   Highlights' `Bookmark.ContentID` points at a *chapter* row; `Bookmark.VolumeID` points at the
   *book* row.
4. **Triggering the Kobo WebKit renderer** (needed for highlighting/bookmarking to work at all)
   = give the file a `.kepub.epub` (or `.fxl.kepub.epub`) extension — official Kobo Labs guidance,
   https://github.com/kobolabs/epub-spec (`README.md` "Sideloading for Testing Purposes").
   ⚠️ The same page also claims sideloaded `.kepub.epub` "will disable bookmarking and note
   keeping" (an old note); the entire existence of kepubify and Calibre's KoboTouchExtended
   plugin (which convert to kepub precisely so highlights work) contradicts that for modern
   firmware. Must be confirmed on 4.38 — see §11.

---

## 5. kepub span conventions (kepubify)

kepubify wraps each sentence/fragment in `<span class="koboSpan" id="kobo.<para>.<seg>">`
(kepubify `kepub/transform.go:540-548`):

```go
func koboSpan(para, seg int) *html.Node {
	return &html.Node{
		...
		Attr: []html.Attribute{
			{Key: "class", Val: "koboSpan"},
			{Key: "id", Val: "kobo." + strconv.Itoa(para) + "." + strconv.Itoa(seg)},
		},
	}
}
```

- `para` increments per paragraph-level element (`p, ol, ul, table, h1..h6`, and images count as
  a paragraph too); `seg` counts spans within the current paragraph (`transform.go:300-400`).
- Example output from kepubify's own tests (`transform_test.go:77-78`):
  `<p><span class="koboSpan" id="kobo.1.1">Test sentence 1. </span>...<span class="koboSpan" id="kobo.1.2"> Replaced. </span>...`
- Kobo's `Bookmark.StartContainerPath` then references those spans as CSS selectors with escaped
  dots: `span#kobo\.5\.2` (boehs.org; samedwardes.com rows `span#kobo\.114\.3`,
  `span#kobo\.115\.1`, `span#kobo\.174\.2`).
- kepubify also adds `div#book-columns > div#book-inner` body wrappers and a `kobostylehacks` style
  element to match official KEPUBs (`transform.go:188-190`, "mandatory" steps: Kobo divs, Kobo
  spans, Kobo styles). Note: official KEPUBs carry it as `<style ... id="kobostylehacks">`
  (comment at `transform.go:259`), while kepubify's own output emits
  `<style type="text/css" class="kobostylehacks">` (`transformContentAddStyle`, `transform.go:264`).
- **Highlighting "doesn't work without" the spans** (kepubify doc comment,
  `transform.go:126-130`): "Highlighting, bookmarking, and other related features don't work
  without this." So the kepubs we push to the device must contain koboSpan ids, i.e. convert with
  kepubify/Calibre-kepub before writing.

---

## 6. Collections: `Shelf` / `ShelfContent` and the "Date added" sort

### 6.1 Tables

Device collections ("shelves") live in `Shelf` + `ShelfContent` (boehs.org table list). Calibre's
KoboTouch driver is the reference implementation for creating them (all SQL below from calibre
`src/calibre/devices/kobo/driver.py`).

**Read shelves:** `SELECT Name FROM Shelf WHERE _IsDeleted = 'false'`
(`get_bookshelflist`, `driver.py:3597`).

**Create a shelf** (modern firmware, `dbversion >= 64` adds `Id` and `Type`;
`check_for_bookshelf`, `driver.py:3665-3693`):

```sql
INSERT INTO "main"."Shelf"
 ("CreationDate","InternalName","LastModified","Name","_IsDeleted","_IsVisible","_IsSynced","Id","Type")
VALUES (?, ?, ?, ?, 'false', 'true', 'false', ?, 'UserTag');
-- values: CreationDate=LastModified=gmtime strftime('%Y-%m-%dT%H:%M:%SZ')
--         InternalName=Name=Id=<shelf name>, Type='UserTag'
```

**Add a book to a shelf** (`set_bookshelf`, `driver.py:3621-3633`):

```sql
INSERT INTO ShelfContent ("ShelfName","ContentId","DateModified","_IsDeleted","_IsSynced")
VALUES (?, ?, ?, 'false', 'false');   -- ContentId = the book row's ContentID
```

(Upsert pattern: first `SELECT _IsDeleted FROM ShelfContent WHERE ShelfName=? AND ContentId=?`,
insert if missing, else `UPDATE ShelfContent SET _IsDeleted='false' WHERE ...`.)

**Remove from shelf / delete empty shelves:** `DELETE FROM ShelfContent WHERE ContentId = ?`
(`driver.py:2840`); `DELETE FROM Shelf WHERE _IsSynced='false' AND ... NOT EXISTS (SELECT 1 FROM
ShelfContent ...)` (`driver.py:3551-3576`, also shows `Type='SystemTag'` rows are skipped — those
are the built-in smart shelves like Shortlist/Wishlist).

### 6.2 Which timestamp drives the "Date added"/"Recent" sort

**Finding (corrected 2026-09-09 for Libra 2 / 4.38.x): it is
`content.___SyncTime`, not `content.DateCreated` — and the sort key must be
Readeck `created`, not `updated`.**

The earlier version of this note claimed `DateCreated` (citing old calibre
behaviour). Direct inspection of the current Kobo Utilities source
(`koboutilities/features/metadata.py:do_update_metadata`, `set_sync_date`
path) and davidfor's Libra 2 statements (MobileRead t=347000) corrects this:

- "Date added" sorts by `content.___SyncTime`; "Recent" sorts by
  `MAX(___SyncTime, DateLastRead)` (was `IFNULL(DateLastRead, ___SyncTime)`).
- Kobo Utilities' "Update metadata in device library" → "Date added" writes
  `___SyncTime` (`set_clause_columns.append("___SyncTime=?")`, sourced from
  calibre Date/Modified/Published or a custom date column). The `DateCreated`
  option in the same dialog is a *separate* "published date" mapping
  (calibre `driver.py:3944-3947`: `pubdate → DateCreated`), shown on the book
  details screen — not the sort key.
- For collections, the "Date added" sort originally used
  `ShelfContent.DateModified` (calibre `set_bookshelf` writes `gmtime()` there),
  broke for a period ("seemingly random but consistent sort"), and was fixed
  to use the book's date-added value — per davidfor, now `___SyncTime` as well.
  Writing the article date to *both* columns covers old and new firmware.
- `content.DateAdded` is the column the Pocket rows populate (MobileRead pocket
  thread query selects `DateAdded`), i.e. it is used for store/Pocket metadata,
  not the sideload sort.

Evidence (pre-correction, kept for history):
- Calibre's metadata sync maps the Calibre *pubdate* to `content.DateCreated`
  (`driver.py:3944-3947`), and maps `content.DateCreated` back to
  `kobo_metadata.pubdate` (`driver.py:2089-2097`).
- davidfor, MobileRead t=333040: "the date used [by the 'Recent' sort] is when
  the book was imported by the device … the Metadata update function in my Kobo
  Utilities plugin can set it"; confirmed by users: after setting it, "sorting
  by 'Date added' in your kobo device will sort your books according to the
  parameter you selected".

**Implication for this project (the 2026-09-09 date-added bug):** the agent
previously set `content.DateCreated = article.updated` once per import. That
was wrong on two axes: wrong column (`___SyncTime` drives the sort) and wrong
timestamp (`updated` moves on every edit/re-fetch, while Readeck's date-added
is `created`). Bulk imports additionally collapsed to the same `___SyncTime`
(import time) and the same `ShelfContent.DateModified` (sync time), so the
Kobo order bore no relation to Readeck's. The fix sets
`content.___SyncTime + content.DateCreated + ShelfContent.DateModified =
bookmark.created` idempotently every pass (created is immutable, so
re-ensuring is harmless and self-heals pre-fix devices). The
`%Y-%m-%dT%H:%M:%SZ` format is what Calibre/Kobo Utilities write.

---

## 7. Import & rescan mechanisms

### 7.1 Programmatic import paths

1. **USB drop + auto-rescan (baseline):** drag the `.kepub.epub` onto the device, eject; Nickel
   imports it automatically (kobolabs/epub-spec "Sideloading"; KoboCloud README relies on the
   same flow). Not wireless.
2. **Wireless: write the file to onboard, then trigger a rescan via NickelDBus.** Both KoSync
   (Go) and KoboCloud (shell) do exactly this:
   - KoboCloud writes into `/mnt/onboard/.add/kobocloud/Library` then runs
     `/usr/bin/qndb -t 3000 -s pfmDoneProcessing -m pfmRescanBooksFull`
     (`src/usr/local/kobocloud/get.sh:126`).
   - KoSync writes into `/mnt/onboard/kosync` (client `config.go:53`) and calls
     `com.github.shermp.nickeldbus.pfmRescanBooks` over D-Bus from Go
     (`client/nickel.go:76-92`).
3. **Targeted sync (preferred for one-off import):** NickelDBus 0.3.0+ exposes
   `n3fssSyncOnboard` / `n3fssSyncSD` / `n3fssSyncBoth` — "a more targeted option to add new
   content compared to pfmRescanBooks / pfmRescanBooksFull. It is what the browser uses when
   downloading ebook files." Signals: `fssGotNumFilesToProcess(int)`, `fssParseProgress(int)`,
   `fssFinished`. (`NDBDbus.cc:720-772`, interface XML
   `src/interface/com.github.shermp.nickeldbus.xml:152-157`.)

### 7.2 Exact NickelDBus invocations

Interface `com.github.shermp.nickeldbus`, object path `/nickeldbus`
(`NickelDBus/README.md:23-26`).

**Qt `qndb` (shipped binary, installed to `/usr/bin/qndb`):**

```sh
qndb -m ndbVersion
qndb -m pfmRescanBooksFull                                  # abbreviated: pfmRescanBooks
qndb -t 30000 -s pfmDoneProcessing -m pfmRescanBooksFull    # rescan and wait for import done (30s)
qndb -t 30000 -s fssFinished -m n3fssSyncOnboard            # targeted onboard sync + wait
```

(`qdoc/pages.qdoc:106-118`; flags `-m/--method`, `-s/--signal`, `-t/--timeout` in ms. KoboCloud
uses the `pfmDoneProcessing` variant; note "even though qndb may time out, the content import
process will not be aborted".)

**From Go (godbus, as KoSync does, `client/nickel.go`):**

```go
conn, _ := dbus.ConnectSystemBus()
obj := conn.Object("com.github.shermp.nickeldbus", "/nickeldbus")
obj.Call("com.github.shermp.nickeldbus.pfmRescanBooks", 0)   // or pfmRescanBooksFull / n3fssSyncOnboard
```

There is also a Go `qndb` port in the repo (`ndb-cli/ndb_cli.go`, uses `godbus/dbus/v5`):
`qndb method -signal pfmDoneProcessing -signal-timeout 30000 pfmRescanBooksFull`.

NickelMenu alternative (no NickelDBus needed, manual):
`nickel_misc :rescan_books` / `nickel_misc :rescan_books_full`
(https://pgaskin.net/NickelMenu/ "Other Nickel stuff"; NickelMenu `src/action_cc.cc:542-567`;
the Kobo-UNCaGED menu example uses `chain :nickel_misc :rescan_books_full`).

### 7.3 One-time install path — `KoboRoot.tgz`

Standard install for NickelMenu/NickelDBus/KoboCloud/KoSync:

1. Download the mod's `KoboRoot.tgz`.
2. Copy it to **`.kobo/KoboRoot.tgz`** on the device (hidden dir; must be a real `.tgz`, Safari
   sometimes renames to `.tar` — KoboCloud README warns about this).
3. Eject / unplug; the device auto-reboots and **extracts the archive over the root filesystem**
   (NickelDBus README; NickelMenu install docs; yingtongli.me "The firmware upgrade process";
   KoboCloud `makeKoboRoot.sh` is literally `tar -cvzf KoboRoot.tgz -C src etc usr`).

Payload layout observed in real release tarballs (this project can ship the same structure):

```
# NickelDBus 0.2.0 KoboRoot.tgz:
./etc/dbus-1/system.d/com-github-shermp-nickeldbus.conf
./usr/bin/qndb
./mnt/onboard/.adds/nickeldbus
./usr/local/Kobo/imageformats/libndb.so        # NickelHook plugin loaded by Nickel

# NickelMenu v0.5.4 KoboRoot.tgz:
./mnt/onboard/.adds/nm/doc                     # example config (NickelMenu reads .adds/nm/*)
./usr/local/Kobo/imageformats/libnm.so         # NickelHook plugin

# KoSync's generated KoboRoot.tgz (client_generator.py:91-99) = usr/ + mnt/ + etc/ subtrees,
#   including mnt/onboard/kosync_client and a NickelMenu config file, plus extracted NickelMenu
#   and NickelDBus tarballs.
```

KoSync's NickelMenu config (a real, working example of wiring a binary to a menu — generated in
`backend/kosync_backend/client_generator.py:53-57`, written to `.adds/nm/doc`):

```
experimental:menu_main_15505_label:KoSync
menu_item :main    :Synchronise        :cmd_spawn          :quiet:/mnt/onboard/kosync_client
```

NickelMenu reads **every regular file in `/mnt/onboard/.adds/nm`** as config
(`NickelMenu/src/config.c:49-100`, `nm_config_files_filter`), so the config can be any filename.
Note: `cmd_spawn` runs `:quiet:` (no output shown); `cmd_output` shows output.

### 7.4 Wi-Fi trigger + NickelMenu fallback

- **udev rule** (KoboCloud, `src/etc/udev/rules.d/97-kobocloud.rules`) — hooks interface-add
  events (Wi-Fi association creates `wlan0`):

  ```
  KERNEL=="eth*", ACTION=="add", RUN+="/usr/local/kobocloud/udev_program.sh"
  KERNEL=="wlan*", ACTION=="add", RUN+="/usr/local/kobocloud/udev_program.sh"
  ```

  `udev_program.sh` re-execs itself with `setsid` ("udev kills slow scripts"), then launches the
  sync job in the background (`KoboCloud/src/usr/local/kobocloud/udev_program.sh:3-19`).
- **NickelDBus network signals** (no udev needed): `wmNetworkConnected`,
  `wmNetworkDisconnected`, `wfmConnectWireless`, `ndbWifiKeepalive`
  (interface XML `:24-27,158-167`; KoboCloud instead polls `ping` in `get.sh:26-36`).
- **NickelMenu fallback** entry to launch the agent manually: a `cmd_spawn` menu item (KoSync
  pattern above). NickelMenu requires firmware ≥ 4.6, tested through 4.31, "safe on any" 4.x,
  **not compatible with 5.x** (pgaskin.net/NickelMenu "About") — fine for 4.38.23697.

---

## 8. On-device execution environment (firmware 4.38 / Mark 7)

Confirmed/precedent facts:

- **Linux armv7l**, kernel 4.1.15-class on Mark 7 (Clara HD `uname -a` in
  yingtongli.me: `Linux (none) 4.1.15-... armv7l GNU/Linux`). Libra 2 is also Mark 7
  (armv7) — ⚠️ kernel details UNVERIFIED for 4.38 specifically.
- **busybox** provides `/bin/sh` and applets including `ping`, `tar`, `ls`, `grep`, `sed`,
  `sleep`, `rm`, `mkdir`, `telnetd`, `ftpd` — KoboCloud's scripts use exactly these
  (`src/usr/local/kobocloud/get.sh` uses `ping`, `tar`, `ls`, `grep`, `sed`, `rm`, `mkdir`;
  MobileRead wiki + yingtongli.me show `inetd.conf` entries
  `23 stream tcp nowait root /bin/busybox telnetd -i` and `ftpd`). ⚠️ Exact busybox applet set on
  4.38 UNVERIFIED (KoboCloud needs a **custom ARM curl** — firmware does not ship curl; see
  below).
- **No curl on the stock firmware** — KoboCloud ships a precompiled ARM `curl` (`curl: ELF
  32-bit ... ARM EABI5 ... dynamically linked ... armhf`; KoboCloud README credits NiLuJe). For
  our agent this doesn't matter: a static Go binary does HTTPS itself.
- **D-Bus is present** (Nickel itself is Qt/D-Bus; `etc/dbus-1/system.d/...` ships in NickelDBus
  tarball).
- **A static Go binary runs on-device** — KoSync ships a Go client built with
  `GOOS=linux GOARCH=arm go build` (client `deploy.py:7-18`; default GOARM=7) that uses only
  pure-Go deps (godbus) and connects to the system D-Bus from the device. Precedent for
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7` static builds (verified working locally, see
  §10; 9 MB static armv7 binary).
- **NickelHook mods load as Qt imageformats plugins** (`/usr/local/Kobo/imageformats/libnm.so`,
  `libndb.so`) — relevant if we ever need to hook Nickel directly; not required for our design.

---

## 9. WAL-safe read recipe + highlight SQL

### 9.1 Canonical "highlights for a volume" SQL

From samedwardes.com (canonical join; this is the pattern also used by kobo-highlights
`main.go:159-168` and october):

```sql
SELECT c.BookTitle,
       c.Title        AS ChapterTitle,
       b.DateCreated,
       b.Text,
       b.Annotation,
       b.Type,
       b.ChapterProgress
FROM Bookmark AS b
LEFT JOIN content AS c ON b.ContentID = c.ContentID
WHERE b.Type IN ('highlight','note')
  AND b.Hidden != 'true'
  AND b.VolumeID = ?
ORDER BY b.DateCreated;
```

Notes:
- Join key: `b.ContentID = c.ContentID` (chapter row). To get the *book* row instead, join
  `c.ContentID = b.VolumeID` (kobo-highlights does this to get the book title).
- `WHERE b.VolumeID = ?` filters one sideloaded kepub (`file:///mnt/onboard/...`).
- Filter `Hidden != 'true'` to skip deleted/pending-delete rows (boehs: Hidden is true when a
  synced annotation is deleted and the deletion needs to sync back).
- Order by `DateCreated` or by reading position (`ChapterProgress`); kobo-highlights orders by
  the paragraph number parsed out of `StartContainerPath`.
- kobo-highlights also requires `Text IS NOT NULL AND LENGTH(TRIM(Text)) > 0`.

### 9.2 WAL-safe reading

Kobo keeps the DB in WAL mode; while Nickel is running, uncheckpointed pages live in
`KoboReader.sqlite-wal`. Reading strategies, from strongest to weakest:

1. **Copy DB + WAL + SHM together, open the copy read-only** — this is what Calibre does
   (`db.py:24-46, 66-83`: on I/O error, `shutil.copyfileobj` the DB and `copy2` the `-wal`
   into a temp file, then open the temp). Works with the device mounted *or* on-device while
   Nickel runs.
2. **`immutable=1` URI** on a copy (or on the file when you know it's quiescent): tells SQLite
   to skip locking/WAL and read the file as-is. Verified working with modernc.org/sqlite (see
   §10). Not safe if a writer is active.
3. **Flip the WAL format flag** — instakobo's trick for WASM sqlite that lacks WAL support:
   copy the DB, set header bytes 18/19 from 2→1 (rollback journal) and open the copy
   (`src/kobo.ts:38-50`). Only sound because Kobo checkpoints on eject; not recommended on-device.
4. **Read directly when USB-ejected**: the DB is checkpointed on unmount, so `file:...?mode=ro`
   works (samedwardes.com uses `duckdb -readonly`; october opens it directly).

### 9.3 Pitfalls

- **`DateCreated` is UTC ISO8601** with `Z` suffix; treat as UTC (boehs row; calibre
  `parse_date(..., assume_utc=True)`). Some rows may use `+00:00` or missing `Z` — parse leniently
  (calibre tries 3 formats + `parse_date`, `driver.py:2089-2097`).
- **`ChapterProgress`** is a float 0–1 (long fractions, e.g. `0.712765957446809`); cast to REAL.
- **`Hidden`** rows must be filtered (see 9.1).
- **Deleted books** leave stale `Bookmark` rows whose `VolumeID` still starts with `file:///` but
  no longer have a matching `content` row — use `LEFT JOIN` and/or check `content` existence.
  October selects `DISTINCT VolumeID FROM Bookmark WHERE VolumeID LIKE '%file:///%'` and then
  reconciles against the filesystem (`bookmark.go:42-47`).
- **Mixed types**: `Hidden` may be `0`/`1` or `'true'`/`'false'`; `IsDownloaded` is text
  `'true'`/`'false'`. Compare defensively (`IN (0,'false')`).
- **Dogear rows** (`Type='dogear'`) are page-turn markers, not highlights; exclude them.

---

## 10. modernc.org/sqlite (pure-Go driver) — feasibility findings

Tested on this box (2026-09-08, Go 1.24.5, linux/arm64), module under `/tmp/dbtest`:

- `go get modernc.org/sqlite@latest` → **v1.58.0 fetches fine**, BUT its `go.mod` declares
  `go 1.25.0` (as does its dependency `modernc.org/libc v1.75.6`). Using it would force a Go 1.25
  toolchain — **incompatible with this repo's Go 1.24.5**.
- Version-scan (clean module, `go 1.24`, `go build` must pass without toolchain download):
  - **v1.46.0 → keeps `go 1.24.0`, builds OK, CGO_ENABLED=0 static, cross-compiles to
    `GOOS=linux GOARCH=arm GOARM=7` (9 MB static ELF, verified).**
  - v1.48.0 and later bump `go.mod` to `go 1.25.0` → avoid until the repo moves to Go 1.25.
  - v1.46.0's dep set: `modernc.org/libc v1.67.6`, `golang.org/x/sys v0.37.0` (the latter
    **already matches** the repo's pinned `golang.org/x/sys v0.37.0`).
- Smoke test passed: in-memory DB, file DB in WAL mode, reopen with `immutable=1`, reopen with
  `mode=ro` — all read back correctly with `database/sql` + `_ "modernc.org/sqlite"` and no cgo.

**Recommendation:** pin `modernc.org/sqlite v1.46.0` when promoting into the root `go.mod`
(test + on-device driver). Used by the fixture generator (see below).

---

## 11. Open questions to verify on the real device later

1. **`Bookmark.ContentID` separator for sideloaded kepubs**: `!!` (october) vs `!ops!`
   (boehs.org). Confirm by highlighting in a sideloaded kepub and dumping the row. Store books
   use `!OEBPS!`.
2. **`StartOffset`/`EndOffset` counting units**: code points vs UTF-16 units; whether offsets are
   relative to the start span only or the whole chapter; whitespace handling. Create known
   highlights and inspect.
3. **"Date added"/"Recent" source columns**: now `content.___SyncTime` (+
   `DateLastRead` for Recent) per §6.2 as corrected — confirm on-device that a
   freshly imported kepub's "Date added" follows `___SyncTime` set to an old
   timestamp, and that the collection sort does too (it should follow either
   `___SyncTime` or `ShelfContent.DateModified`; the agent sets both).
   Also confirm whether `content.DateAdded` matters at all for sideloads.
4. **Whether sideloaded `.kepub.epub` actually supports highlighting on 4.38** (kobolabs/epub-spec
   claims it disables note-keeping; kepubify/KoboTouchExtended assume it works). Test: sideload a
   kepubify-produced kepub, highlight, re-read the DB.
5. **`DbVersion` value on 4.38.23697** (needed to decide `Shelf` column set ≥64).
6. **Shelf row fields Nickel writes**: confirm `Type='UserTag'`, `_IsVisible`/`_IsSynced`
   semantics, and whether `Activity` rows (`Type='Shelf'`) must also be inserted for the shelf to
   appear (calibre deletes orphan `Activity` rows — `driver.py:3576`).
7. **Writing the DB while Nickel is running**: Calibre only writes the DB when the device is
   USB-connected (Nickel idle/absent). On-device, Nickel may hold the DB in WAL mode; verify that
   an agent writing `content`/`Shelf` rows via a second connection (with busy_timeout) is safe,
   or whether writes must happen between a rescan and a reboot, or via a targeted sync.
8. **Exact `content` fields Nickel fills on import** (book row) after `pfmRescanBooksFull` /
   `n3fssSyncOnboard` — MimeType, `IsDownloaded`, `ReadStatus`, `DateCreated`, `BookID` grouping —
   so the agent can safely UPDATE rather than INSERT content rows.
9. **Busybox applet set on 4.38** and whether `wget`/`nc` exist (agent shouldn't depend on them;
   Go does HTTPS).
10. **Whether `n3fssSyncOnboard` (0.3.0+) is present in the NickelDBus release we'd ship**, and
    that `pfmDoneProcessing` fires after it (docs say `fssFinished` for the targeted sync).

---

## Appendix — fixture generator (companion deliverable)

- `tools/kobofixdb/main.go` + `tools/kobofixdb/go.mod` — generates
  `/tmp/kobofixture/KoboReader.sqlite` with the `content` / `Bookmark` / `Shelf` /
  `ShelfContent` / `DbVersion` schema and realistic sideloaded-kepub rows (2 highlights + 1 note,
  `span#kobo\.N.M` paths, a "Readeck" shelf, `DateCreated` in `Z` format). Uses
  `modernc.org/sqlite v1.46.0` (Go 1.24-compatible pin). See
  `docs/research/kobo-fixture-db.md` for mapping to this document.
