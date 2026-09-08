# Kobo Fixture DB — `/tmp/kobofixture/KoboReader.sqlite`

**Stream:** B (highlight-sync) · **Date:** 2026-09-08
**Generator:** `tools/kobofixdb/main.go` (see schema cheat-sheet in
[`docs/research/kobo-db-schema.md`](kobo-db-schema.md) — every fact here cites the same sources).

This is a **synthetic** stand-in for the real `KoboReader.sqlite` until we have a dump from the
target Libra 2 (firmware 4.38.23697). It is meant for offline development and tests of the
highlight-reading and collection/sorting SQL, not for flashing onto a device.

---

## 1. Regenerating the fixture

The generator is a small Go program in its own (nested) module so it can use the pure-Go SQLite
driver without touching the repo's root `go.mod`:

```sh
cd tools/kobofixdb
go run .                     # writes /tmp/kobofixture/KoboReader.sqlite and schema.sql
# or to a custom location (CI/tests):
OUT=/tmp/alt.sqlite go run .
```

Prints a summary and writes two files:

| File | Contents |
|---|---|
| `/tmp/kobofixture/KoboReader.sqlite` | The fixture DB (`content` 7 rows, `Bookmark` 4, `Shelf` 1, `ShelfContent` 2, `DbVersion` 1). |
| `/tmp/kobofixture/schema.sql` | The DDL only — a portable artifact usable with the `sqlite3` CLI (`sqlite3 /tmp/kobofixture/KoboReader.sqlite < schema.sql`) as a no-Go fallback. |

Run the canonical query from `kobo-db-schema.md §9.1` against it:

```sh
sqlite3 -header /tmp/kobofixture/KoboReader.sqlite "
SELECT c.BookTitle, c.Title AS ChapterTitle, b.DateCreated, b.Text, b.Annotation, b.Type, b.ChapterProgress
FROM Bookmark AS b
LEFT JOIN content AS c ON b.ContentID = c.ContentID
WHERE b.Type IN ('highlight','note') AND b.Hidden != 'true'
  AND b.VolumeID = 'file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub'
ORDER BY b.DateCreated;"
```

Expected: the 3 annotations on the Readeck article (2 highlights + 1 note), joined to their
chapter titles, ordered by `DateCreated`.

### Dependency note (modernc.org/sqlite)

- `tools/kobofixdb/go.mod` requires **`modernc.org/sqlite v1.46.0`** — the newest release that
  keeps the repo's **Go 1.24** baseline. `v1.48.0+` declares `go 1.25.0` and would force a
  toolchain upgrade (verified by fetching/building; see `kobo-db-schema.md §10`).
- Verified locally on linux/arm64 with Go 1.24.5: fetches, builds, opens in-memory + WAL +
  `immutable=1` + `mode=ro` DBs, and **cross-compiles static for the device**:
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build` → 9 MB statically-linked armv7 ELF.
- When the sync agent (and tests) start importing this package, promote it into the root
  `go.mod` with `go get modernc.org/sqlite@v1.46.0` and delete the nested `go.mod`/`go.sum`.

---

## 2. What each seeded table represents

### `DbVersion`

```sql
INSERT INTO DbVersion (version) VALUES (126);
```

* Purpose: Calibre reads `SELECT version FROM dbversion` and switches column sets at `>= 64`
  (`calibre .../devices/kobo/db.py:59`, `driver.py:3687-3693`). We model the modern schema
  (with `Shelf.Id`/`Shelf.Type`), so the fixture only needs `>= 64`.
* ⚠️ **The exact 4.38.23697 value is unverified** — see `kobo-db-schema.md §11` (#5).

### `content` — two fake kepubs

**Book A — "Readeck Article One"** (an imported Readeck article, stored under the hidden
`.kobo` area):

| ContentID | VolumeIndex | MimeType | Meaning |
|---|---|---|---|
| `file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub` | -1 | `application/x-kobo-epub+zip` | Book row; `ContentID == VolumeID` (`kobo-db-schema.md §4`). |
| `...kepub.epub!!OEBPS/xhtml/ch001.xhtml` | 0 | `application/xhtml+xml` | Chapter 1 row; `!!` chapter separator (october `utils.go`). |
| `...kepub.epub!!OEBPS/xhtml/ch002.xhtml` | 1 | `application/xhtml+xml` | Chapter 2 row. |
| `...kepub.epub!!OEBPS/xhtml/ch003.xhtml` | 2 | `application/xhtml+xml` | Chapter 3 row. |

Key fields on all A rows: `ContentType=6`, `Attribution='Readeck'`,
`DateCreated=DateAdded='2026-09-01T08:00:00Z'` (the "added" timestamp the device sort reads is
`DateCreated`, `kobo-db-schema.md §6.2`), `___PercentRead=42`, `IsDownloaded='true'` (text bool,
as on the real device).

**Book B — "Project Hail Mary"** (a normal sideloaded kepub in the library path, to prove the
pipeline doesn't special-case the Readeck folder):

| ContentID | VolumeIndex | MimeType |
|---|---|---|
| `file:///mnt/onboard/books/Project Hail Mary - Andy Weir.kepub.epub` | -1 | `application/x-kobo-epub+zip` |
| `...kepub.epub!!OEBPS/text/part0001.xhtml` | 0 | `application/xhtml+xml` |
| `...kepub.epub!!OEBPS/text/part0002.xhtml` | 1 | `application/xhtml+xml` |

Fields: `Attribution='Weir, Andy'`, `DateCreated='2026-08-15T12:30:00Z'`,
`___PercentRead=15`, `IsDownloaded='true'`.

The full column set matches the MobileRead "Backing up Pocket metadata" listing
(https://www.mobileread.com/forums/showthread.php?t=368317) and october's `Content` struct.

### `Bookmark` — 2 highlights + 1 note (+ 1 on Book B)

| ID | VolumeID | ContentID (chapter) | Type | StartContainerPath | Start/EndOffset | ChapterProgress | DateCreated | Notes |
|---|---|---|---|---|---|---|---|---|
| `hlA1` | Book A | ch001 | `highlight` | `span#kobo\.5\.2` → `span#kobo\.5\.2` | 0 → 58 | 0.25 | 2026-09-02T09:15:00Z | Single-span highlight. |
| `hlA2` | Book A | ch002 | `highlight` | `span#kobo\.3\.1` → `span#kobo\.3\.2` | 12 → 40 | 0.50 | 2026-09-02T10:00:00Z | Multi-span (start ≠ end id). |
| `noteA1` | Book A | ch001 | `note` | `span#kobo\.9\.4` → `span#kobo\.9\.4` | 0 → 30 | 0.25 | 2026-09-03T11:00:00Z | `Text` = highlighted text, `Annotation` = the note. |
| `hlB1` | Book B | part0001 | `highlight` | `span#kobo\.12\.1` → `span#kobo\.12\.2` | 0 → 90 | 0.02 | 2026-08-20T18:45:00Z | Exercises per-volume filtering. |

All rows: `BookmarkID` = fixed UUID, `Hidden='false'` (text bool), `StartContainerChildIndex` /
`EndContainerChildIndex` = `-99` (matches boehs.org), `Published='false'`, `ContextString` NULL
(only dogear rows set it), `Version`/`Creator`/`UUID`/`UserID` empty, `DateModified` set.

The `span#kobo\.N.M` paths mirror kepubify's `<span class="koboSpan" id="kobo.N.M">` ids
(`kepub/transform.go:540-548`) as referenced in `Bookmark.StartContainerPath`
(`kobo-db-schema.md §5`).

### `Shelf` + `ShelfContent` — the "Readeck" collection

```sql
-- Shelf (calibre driver.py check_for_bookshelf, dbversion >= 64 shape)
INSERT INTO Shelf (CreationDate, InternalName, LastModified, Name,
                   _IsDeleted, _IsVisible, _IsSynced, Id, Type)
VALUES ('2026-09-08T06:11:37Z','Readeck','2026-09-08T06:11:37Z','Readeck',
        'false','true','false','Readeck','UserTag');

-- ShelfContent (calibre driver.py set_bookshelf); ContentId = book-row ContentID
INSERT INTO ShelfContent (ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
VALUES ('Readeck','file:///mnt/onboard/.kobo/readeck/Readeck Article One.kepub.epub','…','false','false');
INSERT INTO ShelfContent (ShelfName, ContentId, DateModified, _IsDeleted, _IsSynced)
VALUES ('Readeck','file:///mnt/onboard/books/Project Hail Mary - Andy Weir.kepub.epub','…','false','false');
```

This models the shape a future agent should end up writing: one `Shelf` row (`Type='UserTag'` for
a user-created collection) plus one `ShelfContent` row per imported book. The `ContentId` values
are the **book-row** `ContentID`s (== `VolumeID`s), which is what the device uses to resolve
shelf membership.

---

## 3. Mapping to `docs/research/kobo-db-schema.md`

| Fixture element | Schema-doc section |
|---|---|
| `content` columns | §2 |
| Book row vs chapter rows, `VolumeIndex=-1`, MimeTypes | §2, §4 |
| `!!` chapter separator in `ContentID` | §4 |
| `Bookmark` columns, offsets, `span#kobo\.N.M` | §3, §5 |
| `Hidden`, `ChapterProgress`, text bools, UTC `DateCreated` | §3, §9.3 |
| `Shelf`/`ShelfContent` + `DateCreated` drives "Date added" sort | §6 |
| Canonical per-volume highlight query (runnable on this fixture) | §9.1 |
| modernc.org/sqlite pin (v1.46.0, Go 1.24) | §10 |

---

## 4. Known limitations / to reconcile with the real device

See `kobo-db-schema.md §11` for the full open-questions list. The ones that most affect fixture
fidelity:

1. `Bookmark.ContentID` chapter separator `!!` (october) vs `!ops!` (boehs.org) — fixture uses `!!`.
2. Exact `StartOffset`/`EndOffset` counting semantics.
3. `DbVersion` numeric value on 4.38.23697.
4. Whether `Activity` rows must accompany a new `Shelf` for it to appear in the UI.
5. Which `content` fields Nickel fills itself on import (so the agent can UPDATE rather than
   INSERT book rows).
