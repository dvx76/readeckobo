# Mapping & Endpoints — readeckobo server side (stream D)

**Stream:** D (highlight-sync project) · **Date:** 2026-09-08
**Packages:** `internal/readeck` (annotations client), `internal/store` (SQLite state),
`internal/mapper` (range mapping), `internal/app` (agent handlers), `internal/webserver` (routes).
**Consumes:** `internal/kepub` (stream C span map) · **Consumed by:** the on-device agent (stream E)
against the fixed wire contract below.

---

## 1. Wire contract (shared with stream E — do not deviate)

All three routes authenticate either with `Authorization: Bearer <device-token>` or
`?token=<device-token>`; the device token must match a `users[].token` in config
(reuses the existing token→Readeck-token resolution, `app.getReadeckToken`).

| Method & path | Purpose |
|---|---|
| `GET /api/agent/state?device=<serial>` | state feed of the user's non-archived bookmarks (video-type bookmarks excluded) with add/update/remove actions |
| `GET /api/kepub/{id}` | the generated kepub for a bookmark (`application/epub+zip`, attachment `<id>.kepub.epub`) |
| `POST /api/agent/annotations` | ingest device highlight/note rows |

### 1.1 State feed

`GET /api/agent/state?device=<serial>` → 200:

```json
{
  "articles": [
    {
      "bookmark_id": "U9kyUWpFYbYwaXoK8WqLU",
      "title": "…",
      "author": "…",
      "url": "https://readeck.example/api/kepub/U9kyUWpFYbYwaXoK8WqLU?token=<device-token>",
      "etag": "2026-09-08T05:50:51.754739108Z",
      "action": "add|update|remove",
      "updated": "2026-09-08T05:50:51.754739108Z"
    }
  ],
  "next_cursor": null
}
```

- Source: `GetBookmarks(ctx, "", isArchived=false)` — all sites, pagination via `Link rel=next` already handled.
- **Videos excluded**: Readeck's document `type` is `article`, `photo` or `video` (the same
  discriminator as its built-in "Videos" filter). Video bookmarks have no readable text for an
  e-reader, so the state feed filters them out of the current set — a video seen before the
  exclusion (or in the ledger from an older server) is emitted as `remove` once, and
  `GET /api/kepub/{id}` answers 404 for them.
- `url` carries the **device** token as a query param (the agent has no TLS client cert).
- `etag` = the bookmark's `updated` timestamp (`RFC3339Nano`, falling back to `created`,
  then to a hash of URL+title+site). The kepub route falls back to a hash of the article HTML
  only when the bookmark has no timestamp at all.
- **Action semantics** (per-user/device "seen" ledger `articles_seen`, keyed by
  sha256 of the user's Readeck token + device serial):
  - unseen → `add` (ledger row recorded with action `add`);
  - seen, `updated` changed → `update` (ledger row refreshed);
  - seen, `updated` unchanged → the **stored** action is replayed (stable/idempotent);
  - seen, no longer in the non-archived list (archived/deleted now) → `remove`, emitted **once**
    (the ledger row is dropped after the response is computed).
- `next_cursor` stays `null` for v1 — every call returns the full list.

### 1.2 Kepub download

`GET /api/kepub/{id}` → 200 `application/epub+zip`,
`Content-Disposition: attachment; filename="<id>.kepub.epub"`.

- Ownership: `GetBookmarkDetails` (404 → 404). Article served by
  `GetBookmarkArticle`; artifact built with `kepub.Build`.
- Cached in `kepub_cache` keyed by `bookmark_id` + `etag`. When the bookmark carries an
  `updated` timestamp the article is **not** re-fetched on cache hits.

### 1.3 Annotation ingress

`POST /api/agent/annotations`, body:

```json
{
  "device": "<serial>",
  "items": [
    {
      "bookmark_id": "…",
      "bookmark_row_id": "<uuid>",
      "start_path": "OEBPS/xhtml/ch001.xhtml#kobo.2.1",
      "start_offset": 12,
      "end_path": "OEBPS/xhtml/ch001.xhtml#kobo.2.1",
      "end_offset": 25,
      "text": "…",
      "annotation": "note text",
      "type": "highlight|note",
      "date_created": "RFC3339",
      "date_modified": "RFC3339"
    }
  ]
}
```

→ 200:

```json
{
  "results": [
    {
      "bookmark_row_id": "…",
      "status": "created|updated|unchanged|skipped|error",
      "annotation_id": "…",
      "error": "…"
    }
  ]
}
```

Per-item pipeline (a bad item never fails the batch):

1. verify the bookmark belongs to the user (`GetBookmarkDetails`; 404 → `error`);
2. load/refresh the span map for the bookmark (from `kepub_cache` when the etag matches,
   else fetch article + `kepub.Build` and re-cache); the served article HTML is memoized
   per request and also drives L2;
3. map the device range (L1 → L2, §2);
4. fetch the bookmark's existing annotations (`GetBookmarkAnnotations`);
5. dedupe (§3): ledger row → unchanged / PATCH; same-selector overlap → PATCH + ledger-link;
   else POST create (color `yellow`, note from `item.annotation`, ≤1024 runes);
6. record the ledger; emit the per-item status.

---

## 2. Mapping algorithm — L1 then L2

Inputs (per `docs/research/kobo-db-schema.md` §3 and `kepub-generation.md` §8):
device `StartContainerPath`/`EndContainerPath` carry the kepub span id as a fragment
(`OEBPS/xhtml/ch001.xhtml#kobo.2.1`, or CSS-escaped `span#kobo\.2\.1`, or URL-escaped `%23`);
`StartOffset`/`EndOffset` are rune offsets **within the respective span's text**.

### L1 — exact (preferred)

1. `ParseSpanID` extracts `kobo.<block>.<run>` from each path (`#`/`%23`/CSS-escaped dots handled).
2. Both span ids must resolve in the kepub span map and **share a block selector**
   (single-block highlights, including multi-run same-block ranges).
3. Offsets (runes into the block's `ElementText`):
   - `start = PrefixRunes(startSpan) + StartOffset`
   - `end   = PrefixRunes(endSpan)   + EndOffset`   (end exclusive)
   - both clamped to `[0, runes(ElementText)]`; `end ≤ start` → skip.
4. Selector = the recorded `SpanRef.Selector`; device `text` is passed through
   (Readeck re-computes the stored text from its own DOM anyway).

### L2 — text fallback

Used when L1 fails (unknown span id, or cross-block start/end):

1. Normalize whitespace on both sides (runs → single space, trim) and locate the device text
   in the **current served article HTML**: first occurrence wins.
   When the start span id *did* resolve (cross-block case), the start block is searched first.
2. The found block element gets a body-relative selector in Readeck's dialect
   (`tag[i]` positional steps, `tag[@id='…']` preferred for document-unique ids —
   the same conventions as `internal/kepub`).
3. Offsets: the rune range of the matched text inside the block's raw concatenated text
   (position restored through the whitespace-collapse map), end exclusive, clamped.

Both fail (cross-block *text* that no single block contains, or text drift) → **skipped**
with a reason. There is no L3: Readeck's API rejects text-only payloads
(`docs/research/readeck-annotations-api.md` §13).

Verified against the real fixture (`internal/kepub/testdata/readeck-article.html`):
L1 for single-span and multi-span same-block ranges (incl. nested `<strong>`/`<em>`),
end-clamping, L2 via start-block preference and via document-wide search, whitespace
normalization (newlines/double spaces), missing-span-skip and cross-block-text-skip.

---

## 3. Dedupe / conflict rules

1. **Ledger row exists** (`annotation_ledger`, keyed `device` + `bookmark_row_id`):
   - `date_modified` unchanged → `unchanged` (no API call);
   - `date_modified` changed → `PATCH /annotations/{id}` with `{color:"yellow", note}` → `updated`.
2. **No ledger row, but the mapped range overlaps an existing annotation**
   (simple interval overlap on the same selector) → PATCH that annotation's color/note and
   ledger-link the device row to it → `updated`. This absorbs Readeck's own geometric dedup
   (400 `overlapping annotation` on a re-POST of the same range).
3. **Otherwise** → `POST create` → `created`, ledger-linked.

Colors are **not mapped yet** — everything is `yellow` (Readeck accepts any string ≤32 chars;
the device has no color field). Notes are trimmed and capped at 1024 runes
(Readeck's `max_len:1024`).

---

## 4. State store (SQLite, `internal/store`)

- Driver: `modernc.org/sqlite v1.46.0` (pure Go, pinned in root `go.mod`; v1.48+ needs Go 1.25 —
  see `kobo-db-schema.md` §10). WAL mode + `busy_timeout`/`synchronous=NORMAL` per connection
  via DSN `_pragma=` parameters. Store file: `<server.data_dir>/readeckobo.sqlite`
  (new optional config key `server.data_dir`, default `data`; `.mem`-style `:memory:` for tests).
- Tables:

```sql
articles_seen(user_token_hash, device, bookmark_id, updated, action,
              PRIMARY KEY (user_token_hash, device, bookmark_id));
kepub_cache(bookmark_id PRIMARY KEY, etag, epub BLOB, span_map TEXT, created_at);
annotation_ledger(device, bookmark_row_id, bookmark_id, annotation_id,
                  date_modified, updated_at,
                  PRIMARY KEY (device, bookmark_row_id));
```

Note: `annotation_ledger` uses the composite primary key `(device, bookmark_row_id)` — the
draft spec's bare `bookmark_row_id PRIMARY KEY` would make multi-device setups collide.

`user_token_hash` = sha256 hex of the user's Readeck token (never stored in plaintext).
Methods are plain `database/sql` upserts/selects (`ON CONFLICT … DO UPDATE`).

---

## 5. Routes

Registered in `internal/webserver/webserver.go` (Go 1.22+ patterns):

```
GET  /api/kepub/{id}        → app.HandleKepubDownload
GET  /api/agent/state       → app.HandleAgentState
POST /api/agent/annotations → app.HandleAgentAnnotations
```

---

## 6. Limitations & open questions (real-device validation TODO)

1. **Offset units on real firmware** — `StartOffset`/`EndOffset` on the device are assumed to be
   runes within the span's text (stream B §11.2 UNVERIFIED). If Nickel counts UTF-16 units or
   includes/snaps whitespace differently, L1 offsets shift; L2 then only helps when the *text*
   still matches. Needs a fixture device dump against known highlight positions.
2. **`DateModified` ledger keying** — the ledger compares `item.date_modified` strings verbatim.
   If the device rewrites timestamps between syncs (or the agent truncates them), rows could
   flip to `updated` spuriously (harmless — PATCH is idempotent) or miss a real edit.
3. **Cross-block highlights** — a single Readeck annotation cannot span two block elements
   (element-scoped ranges), so cross-block text is skipped unless it happens to sit entirely
   inside one block. Splitting into per-block annotations (kepub-generation.md §8) is a TODO.
4. **Text drift** — L2 matches whitespace-normalized text; editorial re-extraction or
   dynamically rendered content that changes the article text leaves L2 unmapped → skipped.
   `kepub_cache` is keyed by etag (bookmark `updated`), so a re-extracted article invalidates
   the span map and re-mapping happens automatically — but only for *new* syncs.
5. **Id-prefix stability** — Readeck rewrites element ids per bookmark (`uQ.GbYH.*`). Selectors
   recorded against one served fragment may not resolve after re-extraction if the prefix
   changes (`readeck-annotations-api.md` §4); positional selectors (`p[3]`) are prefix-proof.
   The kepub span map is re-derived from the currently served article on every etag change, so
   recorded selectors always come from the same fragment they are validated against.
6. **`has_article=false` bookmarks** — the state feed includes every non-archived bookmark per
   the contract, except `type="video"` bookmarks, which are excluded (no readable text for an
   e-reader); a bookmark without extracted article content still produces an empty-kepub
   (0 spans). Filtering by `has_article` (photos included) is a candidate follow-up if the
   agent chokes on empty chapters.
7. **Per-bookmark annotation list pagination** — `GetBookmarkAnnotations` does a single GET;
   Readeck's per-bookmark list appears unpaginated in practice (the *global* list is paginated).
   If a bookmark accumulates >50 highlights, verify whether pagination kicks in.
8. **Removal is one-shot** — `remove` is emitted once (ledger row dropped). If the agent misses
   the response, the stale kepub stays on device; a future "confirm removal" flow
   (or keep-emit-until-acked) is a TODO.
9. **Colors/notes mapping** — colors are hardcoded `yellow`; notes map 1:1. Kobo has no color
   field today, but per-row note/color precedence on device (Type `note` vs `highlight`) and
   Readeck PATCH immutability of selectors (fixes require DELETE + re-POST) are untested live.
10. **Real-device validation** — end-to-end test plan: sideload a fixture-generated kepub
    (`tools/kobofixdb`), highlight across spans/paragraphs, run the agent against this server,
    then inspect created annotations in Readeck and confirm selectors resolve in the web reader.