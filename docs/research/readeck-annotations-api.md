# Readeck Annotations API — Empirical Spike Findings

**Readeck version tested:** 0.23.2 (standalone binary `readeck-0.23.2-linux-arm64`, sha256
`bdc4323611aefc40d2152c3e4c7f062b44a68b081902ef0059dd7527b52f6fcb`)
**Date:** 2026-09-08 · **Method:** live instance on `127.0.0.1:8017`, API token auth, curl records saved under `/tmp/readeck-spike/results/`.
**Fixture article:** `/tmp/readeck-spike/fixture.html`, served from `http://127.0.0.1:8018/fixture.html`.
**Bookmark used:** `U9kyUWpFYbYwaXoK8WqLU` (all ids below are instance-local).

---

## 1. Setup notes (repeatable in CI/locally)

1. Download + verify the standalone binary (see sha256 above), `chmod +x`.
2. `config.toml` (all paths under `/tmp` in this spike):

   ```toml
   [main]
   log_level = "debug"
   log_format = "text"
   secret_key = "<long random string>"
   data_directory = "/tmp/readeck-spike/data"
   [server]
   host = "127.0.0.1"
   port = 8017
   [database]
   source = "sqlite3:/tmp/readeck-spike/data/db.sqlite3"
   [extractor]
   denied_ips = []          # REQUIRED: default denies 127.0.0.0/8, so local fixture fetches would fail
   ```
3. Start: `nohup ./readeck serve -config config.toml > readeck.log 2>&1 &`; wait for `server started`.
4. Create user (no web UI needed):
   `./readeck user -u spike -password 'spike-pass-1234' -json` → `{"status":"create",...}`.
5. **API token has no CLI command in 0.23.2.** Log in through the web form to get a session cookie:
   `curl -c cookies.txt -X POST http://127.0.0.1:8017/login -d 'username=spike&password=...'` (303).
   Then `curl -b cookies.txt -X POST http://127.0.0.1:8017/profile/tokens` (303 → `/profile/tokens/<token-id>`);
   the token value is shown **once** on `GET /profile/tokens/<token-id>`.
   Verify: `curl -H "Authorization: Bearer <TOKEN>" /api/profile`.

---

## 2. API documentation location

- Readeck serves its own API docs: **`GET /docs/api`** (HTML, Rapidoc). The machine-readable spec is
  **`GET /docs/api.json`** (OpenAPI 3, ~200 KB). Both need a session cookie or an API token.
  Saved copy: `/tmp/readeck-spike/openapi.json` (also `readeck-spike/docs/research/` can host a copy if needed later).
- The generated OpenAPI spec is **partially stale** vs 0.23.2 source + live behavior:
  - `annotationCreate` in the spec omits `note` — the API accepts it (see §6).
  - `annotationUpdate` in the spec lists only `color` — the API also accepts `note`.
  - `annotationInfo` in the spec omits `color` and `note` — live responses include both.
  - `annotationSummary` (global list) in the spec omits `color`/`note` — live responses include both.
  - Spec says POST create requires `start_selector, start_offset, end_selector, end_offset, color` — **live matches** (no `text` field!).
- **Reading progress has no dedicated route.** It is `PATCH /api/bookmarks/{id}` with `read_progress` (0–100) and/or `read_anchor` (CSS selector string, ≤256 chars), both documented in `bookmarkUpdate`.

---

## 3. Relevant endpoints (as served by 0.23.2)

| Method | Path | Status codes observed |
|---|---|---|
| GET | `/api/bookmarks/{id}/annotations` | 200 |
| POST | `/api/bookmarks/{id}/annotations` | 201, 400, 404, 422, 500 |
| PATCH | `/api/bookmarks/{id}/annotations/{annotation_id}` | 200, 404, 422 |
| DELETE | `/api/bookmarks/{id}/annotations/{annotation_id}` | 204, 404 |
| GET | `/api/bookmarks/annotations` | 200 (paginated, all user highlights) |
| GET | `/api/bookmarks/{id}/article` | 200 text/html fragment |
| GET | `/api/bookmarks/{id}/article.epub` | 200 `application/epub+zip` |
| GET | `/api/bookmarks/{id}/article.md` | 200 `text/markdown` (spec; not exercised) |
| PATCH | `/api/bookmarks/{id}` | 200, 422 (progress/anchor/note/labels/etc.) |

### 3.1 Annotation JSON on the wire (all fields, exact order as returned)

Create (201) and GET list (200) item:

```json
{
  "id": "RBdkrpkM5GmHfZzgRdRKNR",
  "start_selector": "/section/article/p[1]",
  "start_offset": 0,
  "end_selector": "/section/article/p[1]",
  "end_offset": 55,
  "color": "yellow",
  "created": "2026-09-08T05:50:51.754739108Z",
  "text": "ALPHA one two three four five six seven eight nine ten.",
  "note": ""
}
```

There is **no `updated` field on annotations** (only on bookmarks). `note` is always present (empty string when unset).

### 3.2 POST create body (what's actually validated)

Required: `start_selector`, `start_offset` (≥0), `end_selector`, `end_offset` (≥0), `color` (≤32 chars).
Optional: `note` (≤1024 chars, trimmed). **No `text` field exists** — a `text` key in the body is silently ignored
and the server computes the highlighted text from its own article DOM. Unknown keys are ignored elsewhere too
(e.g. `{"progress":42}` on bookmark PATCH returns 200 with `{"href","id"}` and changes nothing).

### 3.3 GET list ordering

Per-bookmark list is sorted **by document position** of the range, not by creation time
(the FOXTROT annotation created before ECHO appears after it in the list).
Global list `/api/bookmarks/annotations` is creation-time **descending** and paginated
(`Total-Count`, `Current-Page`, `Total-Pages`, `Link` headers).

### 3.4 Global list item (`annotationSummary`) fields

`id, href, text, created, color, note, bookmark_id, bookmark_href, bookmark_url, bookmark_title, bookmark_site_name`

---

## 4. Selector strictness verdict — THE core question

**Verdict: Readeck validates selectors and offsets against its own extracted article DOM at write time and refuses
anything that doesn't resolve; it does not store blindly.** A selector must be an **XPath expression that resolves
relative to the article `<body>`** (`./` is prepended server-side). It must target an element; the offsets are
rune (Unicode code point) offsets into the concatenation of *all descendant text nodes* of that element.
Non-overlap with existing highlights is enforced; the text content is derived, never client-supplied.

### Dialects tried (task order 8a→8d), exact status/body

| # | Payload (`start_selector`/`end_selector`) | Result |
|---|---|---|
| 8a | `p` (CSS-ish name, plus extra `text` key) | **400** `{"status":400,"message":"element \"p\" not found"}` |
| 8b | `/body/article/p[1]` (root-absolute XPath) | **400** `element "/body/article/p[1]" not found` (resolves to `.//body/…`; `body` is an ancestor of the query root) |
| 8b′ | `/section/article/p[1]` (leading slash, but body-descendant) | **201** ✅ |
| 8c | `//p[@id='uQ.GbYH.p-bravo']` (double-slash XPath w/ rewritten id) | **500** `Internal Server Error` (server log: `expression must evaluate to a node-set`; `.//`+`//` concatenation is not a valid node-set expression — a server bug, not a 4xx) |
| 8c′ | `section/article/p[@id='uQ.GbYH.p-bravo']` (id predicate, no leading `//`) | **201** ✅ |
| 8c″ | `section[1]/article[1]/p[3]` (Readeck's own positional dialect) | **201** ✅ |
| 8d | `""` (empty selectors) | **422** `field is required` for each selector (text-only POST is impossible) |

Additional probes:

- `p[1]` alone → 400 (no `<p>` is a direct child of `<body>`).
- Valid selector, `end_offset=999` → **400** `index "999" is out of range`.
- start offset > end offset on one text node → 400 `invalid range` (from source; same error family as above).
- reversed start/end elements → 400 `no text nodes in range` (source).
- same range as an existing highlight → **400** `overlapping annotation` (see §8).

### What a working selector looks like

- **Readeck's own canonical dialect** (what its web reader sends; `web/src/controllers/annotations_controller.ts`,
  function `getSelector`): body-relative positional path, `tag[position]/tag[position]/…`, e.g.
  `section[1]/article[1]/p[1]`. The Go server evaluates it as XPath `./section[1]/article[1]/p[1]`.
- Any XPath that is valid **with `./` prepended**, e.g. `section/article/p[2]` (positional, no indices),
  `section/article/p[@id='uQ.GbYH.p-bravo']` (attribute predicate), `/section/article/p[1]`.
- `//…` prefix never works (500). Absolute-from-document-root paths (`/html/…`, `/body/…`) never resolve.

### Offsets semantics (verified empirically + source `pkg/annotate/annotate.go` `getTextNodeBoundary`)

- Offsets count **runes**, over ALL descendant text nodes of the selected element (nested `<strong>`/`<em>` text counts).
  Proof: BRAVO paragraph `BRAVO <strong>bold inside</strong> and <em>italic inside</em> plus more words here.`
  with start_offset 6 / end_offset 35 returned `text: "bold inside and italic inside"`.
- `start_offset` is a cursor into the element's concatenated text; a start landing exactly at a node boundary
  advances to the next descendant text node.
- `end_offset` is exclusive at the character level, but setting it to the element's total rune length still yields
  the final character (offset lands at the last node's end). Proof: FOXTROT annotation with
  `end_offset = 61` (length 62, trailing `.` excluded) rendered as `<rd-annotation>…oscar</rd-annotation>.` in the article.
- **The element ids in the article are rewritten per bookmark**: `id="p-alpha"` in the source becomes
  `id="uQ.GbYH.p-alpha"` in one bookmark and `id="Wy.DsfP.p-alpha"` for a second bookmark of the *same URL*.
  Clients therefore **must** derive selectors from the article fragment served by `GET /api/bookmarks/{id}/article`
  (or use positional selectors), never from the original HTML or from ids in a locally transformed copy.

### The article DOM positions

Readeck extraction wraps content in `<section><article>…</article></section>` (inside `<body>`), strips
classes, hoists some wrappers (`<div>` dropped), keeps `id` attributes rewritten with the per-bookmark prefix,
and keeps nested `<strong>`/`<em>`/`<ul>`/`<li>`/`<blockquote>` markup.

---

## 5. Colors — no enum, required

- Accepted and stored verbatim (all ≤32 chars): `yellow`, `red`, `green`, `blue`, `cyan` (arbitrary), `none`, `orange`, `pink`-family strings.
- **There is no permissive default via the API**: omitting `color` (or sending `""`) → **422** `field is required`.
- The UI palette is a frontend concern; the API accepts any string.
- Empty color that somehow reaches storage is rendered as `yellow` in the article/EPUB markup (model-level fallback, `internal/bookmarks/annotations.go`), but the API form prevents that.

## 6. Notes — supported in 0.23.2 API

- POST with `"note":"This is my spike note"` → 201, read back verbatim in list/global list responses.
- Also stored in the article fragment as `title="…"` on an extra empty `<rd-annotation data-annotation-note="" …>` sibling
  (`dataset.AnnotationCallback`), and surfaced in the web UI.
- Max length 1024 (`validate:"trim max_len:1024"`); over-length → 422.
- (Annotation notes are distinct from **bookmark** notes: `PATCH /api/bookmarks/{id} {"note":"…"}` sets the bookmark-level
  Markdown note, field `note` on `bookmarkSummary`/`bookmarkUpdate`.)

## 7. PATCH semantics

`PATCH /api/bookmarks/{id}/annotations/{annotation_id}`:

- Only **`color` (required) and `note` (optional)** are bound. Selectors, offsets, and `text` in the PATCH body are
  **silently ignored** — the stored selectors/offsets stay untouched (verified: PATCH with `start_selector:"section/article/p[99]"`
  left the annotation's selectors and offset and text 100% unchanged).
- `{"color":"purple"}` → 200 `{"annotations":[<full DOM-ordered list>],"updated":"<RFC3339Nano>"}`.
- `{"color":"purple","note":"patched note"}` → 200 (note replaced on the annotation).
- `{"note":"note only"}` → **422** `field is required` (color can't be omitted).
- Unknown annotation id → 404 `Not Found` (plain text). Unknown bookmark id → 404.

## 8. DELETE + duplication behavior

- DELETE → **204 No Content**, empty body. Second DELETE (already deleted) → 404. List afterwards no longer contains it.
- **No content-based dedup, but geometric dedup:** POSTing the exact same range twice — second POST → **400**
  `{"status":400,"message":"overlapping annotation"}`. Two non-overlapping ranges in the same paragraph co-exist;
  the overlap check runs against the article DOM with existing highlights re-applied (`annotate.AddAnnotation` →
  `ToRange` validator in `pkg/annotate/annotate.go`).
- Because the overlap check keys on the *text range*, two annotations with identical text but in different
  paragraphs are both accepted.

## 9. Error/validation shape

| Case | Status | Body |
|---|---|---|
| Form validation (missing/empty required field) | 422 | `{"is_valid":false,"errors":null,"fields":{<every field with is_null,is_bound,value,errors>}}` |
| Malformed JSON | 422 | same shape with `"errors":["invalid input data"]` (note: **not** 400) |
| Selector doesn't resolve | 400 | `{"status":400,"message":"element \"<selector>\" not found"}` |
| Offset out of range | 400 | `{"status":400,"message":"index \"<n>\" is out of range"}` |
| Range spans nothing / reversed | 400 | `{"status":400,"message":"no text nodes in range"}` / `"invalid range"` |
| Overlap | 400 | `{"status":400,"message":"overlapping annotation"}` |
| XPath `//` syntax | 500 | `Internal Server Error` (server bug: `expression must evaluate to a node-set`) |
| Unknown bookmark or annotation | 404 | `Not Found` (text/plain) |

## 10. Reading-progress endpoint (discovery)

- **`PATCH /api/bookmarks/{id}` with `{"read_progress": 42}` → 200**
  `{"href":…,"id":"…","read_progress":42,"updated":"…"}`; verified persisted via GET bookmark (`read_progress: 42`, plus `read_anchor` when set).
- `{"read_progress": 101}` → 422 `"must be lower or equal than 100"`; min 0 (`gte:0`).
- `{"read_anchor": "section/article/p[2]"}` → 200, persisted (CSS-selector-ish string, ≤256 chars).
- The phantom key `{"progress": 42}` → 200 `{"href","id"}` (unknown field silently ignored — no error, no change).
- Full `bookmarkUpdate` field set visible in the 422 dump: `title, description, site_name, authors, published, lang,
  text_direction, note, is_marked, is_archived, is_deleted, read_progress, read_anchor, labels, add_labels, remove_labels, _to`.

## 11. Article & EPUB rendering of highlights

- `GET /api/bookmarks/{id}/article` (fragment, `text/html`): highlights wrapped in
  `<rd-annotation id="annotation-<aid>" data-annotation-id-value="<aid>" data-annotation-color="<color>">`; notes add an
  extra empty `<rd-annotation data-annotation-id-value=… data-annotation-note="" title="<note>" data-annotation-color=…>`.
- `GET /api/bookmarks/{id}/article.epub` (`application/epub+zip`): highlighted text becomes `<mark>` elements;
  annotations **with a note** add a `noterefN` anchor and a footnote `<aside id="noteN" epub:type="footnote">` in a
  trailing “Notes” section; highlights without notes appear as plain `<mark>` without footnotes
  (verified: 2 note annotations → 2 footnotes out of 7 highlights).
- `with_notes` query parameter exists only on `GET /api/bookmarks/{id}/share/link`, not on the article routes.

## 12. Source files inspected (Codeberg, tag `0.23.2` unless noted)

- `internal/bookmarks/annotations.go` — model `BookmarkAnnotation` (JSON keys incl. `note`; DB `Value()` yellow fallback).
- `internal/bookmarks/routes/forms_annotations.go` — `annotationForm`/`annotationUpdateForm` validation rules; create flow.
- `internal/bookmarks/routes/api_bookmarks.go` — handlers `annotationCreate/Update/Delete`, `bookmarkAnnotations`, `annotationList`,
  `bookmarkExport` (lines ~360–460, ~125–165).
- `internal/bookmarks/routes/x-annotations.templ` — web highlights page.
- `pkg/annotate/annotate.go` — selector resolution (`htmlquery.Query(root, "./"+selector)`), rune offsets,
  overlap + range checks, `getSelector` canonical `tag[i]/tag[i]` output.
- `internal/bookmarks/dataset/annotations.go` — `AnnotationCallback` (rd-annotation markup incl. note titles).
- `internal/bookmarks/dataset/bookmarks.go` — `GetArticle` with annotation application.
- `internal/bookmarks/converter/epub.go` — EPUB `<mark>` + footnote generation.
- `web/src/controllers/annotations_controller.ts` (branch `main`) — frontend `getSelector` matching the Go one; POST/PATCH/DELETE calls.
- DB migrations `internal/db/migrations/{sqlite3,postgres}/03_bookmark_annotations.sql`, `27_annotation_fts.sql`.

## 13. Mapping-ladder recommendation (evidence-based)

Context: we push Kobo highlights into Readeck; we have a locally transformed copy of the article, but
Readeck will only accept ranges it can resolve against **its own** extracted DOM.

- **L1 — Exact selectors (preferred):** fetch `GET /api/bookmarks/{id}/article` (the fragment), parse it in the same
  engine semantics as Readeck (or simply locate the text), and compute body-relative XPath selectors in Readeck's
  canonical dialect (`section[1]/article[1]/p[1]`, positional or `p[@id='<rewritten-id>']`, NEVER `//`-prefixed),
  with rune offsets over the element's concatenated text. POST create with those; expect 201. Conflicts with
  existing highlights are signalled with 400 `overlapping annotation` (skip/merge then).
  → This works today; it is exactly what the web reader does.
- **L2 — Text-match fallback:** when a range can't be located exactly (e.g. our transformed copy diverges from
  Readeck's extraction), find the paragraph whose text (normalized per Readeck's extraction, including nested
  element text) contains the Kobo highlight text, and set selectors to that paragraph with offsets computed
  against the served article text. Server recomputes `text` from the DOM, so compare
  server-returned `text` to the Kobo text and post-adjust (or accept the paragraph-level approximation).
  Because selectors/offsets are immutable after creation (PATCH ignores them), any fix requires DELETE + re-POST.
- **L3 — Text-only is NOT possible:** the API has no text-only mode (422 — `start_selector`/`end_selector`/offsets/`color`
  are required, `text` is ignored). The minimum viable fallback for a highlight whose text can't be matched is a
  paragraph-level highlight of the *nearest containing* paragraph (L2 at paragraph granularity), or skipping it.
  A literal L3 “just send the text” is rejected on every attempt.

Additional practical notes:

- Ids in the served article are rewritten per bookmark (random prefix), so cache `article` + selectors together, and
  re-derive after Readeck re-extracts.
- Offsets are runes; count graphemes as runes and beware of normalization differences (Readeck's extraction may
  normalize whitespace; match on collapsed whitespace).
- Overlap detection makes blind client-side dedup unnecessary — the server rejects duplicates; but the 400 must be
  interpreted as “already highlighted” rather than a failure of our selector format.