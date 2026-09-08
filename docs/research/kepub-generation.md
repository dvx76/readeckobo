# Kepub Generation — span map & mapping contract

**Stream:** C (highlight-sync project) · **Date:** 2026-09-08
**Package:** `internal/kepub` (stdlib + `golang.org/x/net/html` only)
**Inputs:** `docs/research/readeck-annotations-api.md` (stream A, live-verified Readeck
0.23.2 API behavior) and `docs/research/kobo-db-schema.md` (stream B, Kobo device schema).

---

## 1. Purpose

`kepub.Build(articleHTML, meta)` produces a Kobo-readable `.kepub.epub` from article HTML and,
together with it, an explicit **span map** (`Artifact.Spans` / `Artifact.SpanMapJSON`) that makes
every device-side highlight anchor position-mappable back into Readeck annotation ranges.

The bridge is the `kobo.N.M` span id convention:

1. The kepub we generate wraps each text run in
   `<span class="koboSpan" id="kobo.<block>.<run>">` (kepubify convention, see stream B §5).
2. Kobo stores highlights against those span ids: `Bookmark.StartContainerPath` /
   `EndContainerPath` look like `span#kobo\.9\.1` and `StartOffset`/`EndOffset` are offsets
   inside the *span's* text (stream B §3).
3. The span map tells the server-side mapping engine which Readeck block each span lives in,
   and at which rune offset within that block's text the span starts. That yields (with the
   Readeck-served article) the selectors/offsets the Readeck API accepts (stream A §4).

## 2. Input contract

`Build` accepts HTML in Readeck's extraction shape — the article fragment served by
`GET /api/bookmarks/{id}/article`:

```html
<section><article>
<p id="uQ.GbYH.p-alpha">ALPHA one two three four five six seven eight nine ten.</p>
...
</article></section>
```

Notes:

- Ids in the served fragment are **per-bookmark rewritten** (`uQ.GbYH.*`); the transform keeps
  them verbatim and prefer them in selectors (see §5). Selectors are therefore only valid for
  the bookmark they were derived from — clients must cache `article` + span map *together* and
  re-derive after Readeck re-extracts (open questions §9.1).
- The fragment may contain Readeck's highlight markup:
  `<rd-annotation id="annotation-…" data-annotation-color="…">…text…</rd-annotation>` plus
  optional empty note-marker siblings `<rd-annotation data-annotation-note="" title="…">`.
- Any element whose tag starts with `rd-` is treated as Readeck annotation chrome.

## 3. HTML transform (in document order)

### 3.1 rd-* wrappers are rewritten into styled spans

Every `rd-*` element is rewritten into a `<span class="rd-…">` element (full rule in §9.5):
the tag becomes `span`, the original (lowercased) tag name becomes the class
(`rd-annotation`, …), and the remaining attributes (`id`, `data-annotation-*`, `title`) plus
children stay in place. Consequences:

- Text is unchanged (wrappers carry no text); the annotation text stays exactly where it was,
  now inside a styled span.
- The class hooks the `.rd-annotation` stylesheet rule, so pre-existing Readeck highlights
  render as highlighted spans on the device (see §9.5).
- Empty note-marker wrappers become empty spans carrying no visible text (their `title` note
  is not preserved in the kepub — Readeck already stores it server-side; out of scope here).
- The styled span is an inline element, so annotated text becomes its own inline run: run
  segmentation around annotated text may differ from the annotation-free article. E.g.
  `<p><rd-annotation>FOXTROT … oscar</rd-annotation>.</p>` becomes two runs — the inline run
  `FOXTROT … oscar` (inside `<span class="koboSpan">`), then the text run `.` — where the
  annotation-free paragraph would be a single run.

The wrappers are kept (as styled spans) precisely so pre-existing Readeck highlights render on
the device. Trade-off: run segmentation around annotated text may differ from the
annotation-free article (annotated text becomes its own inline run). The per-block invariants
still hold — text coverage and the selector set are identical with and without annotations
(tests assert block-coverage equality, see `TestRealFixtureAnnotationTransparency`).

### 3.2 Block containers and runs

**Container set** (the elements whose text runs get wrapped, exactly):

`p, li, h1..h6, blockquote, dd, dt, td, th, figcaption, div`

Deliberate exclusions, all documented assumptions:

- `section`, `article`, `ul/ol`, `table`, `tr`, `figure`, `dl` — structural wrappers that
  normally carry no direct text; they still appear (with positional steps) in selectors
  pointing at their descendants.
- `pre`/`code` — code blocks get **no spans**, so they cannot be highlighted on device.
  This is a known limitation, see §9.4.
- Images/videos — they carry no text; nothing to anchor.

A container is processed iff it has a **non-whitespace text character outside any nested
container** (`hasWrappableText`). Empty or whitespace-only containers (`<p></p>`, `<div> </div>`)
are left untouched and do not consume block numbers.

**Run definition** — the unit Kobo anchors highlights to:

- A *text run*: a maximal sequence of consecutive direct-child text nodes.
- An *inline run*: a maximal sequence of consecutive direct-child inline elements (any element
  not in the container set whose subtree contains no container element); the run's text is the
  concatenation of the whole inline subtree(s).
- Text and inline runs never merge with each other (`<p>a<em>b</em>c</p>` → runs `a | <em>b</em> | c`).
- Comments/doctypes/other nodes end a run; they are not wrapped and carry no text.
- Runs with empty text (e.g. `<img>`, `<br>` between text runs) produce no span; surrounding
  text runs remain exactly adjacent in the text.

**Invariants (tested):**

1. Spans carry no text — the document's normalized (whitespace-collapsed) text is identical
   before and after the transform.
2. Within every processed container, each text character is covered by exactly one span:
   spans are direct children of the container, in document order, and their texts tile the
   container's text exactly (text inside nested containers is excluded — it belongs to those
   containers). Inter-block whitespace (between paragraphs, list items) is *not* inside any
   span: it lives at the level of structural wrappers, where no highlight can be anchored
   anyway.
3. Span ids are unique and follow `kobo.<block>.<run>`: `block` is a 1-based counter over
   containers that received at least one span, in document order; `run` restarts at 1 per
   container.
4. The serialized chapter is XML-well-formed (self-closed void elements, only `&amp; &lt;
   &gt; &quot;` entities — no `&nbsp;`-style HTML entities; non-breaking spaces and any
   Unicode are written raw as UTF-8), so it parses with `encoding/xml`, EPUB readers, and
   `x/net/html` alike.

Example output for the spike article's paragraph 2 (5 runs around the strong/em):

```html
<p id="uQ.GbYH.p-bravo">
  <span class="koboSpan" id="kobo.2.1">BRAVO </span>
  <span class="koboSpan" id="kobo.2.2"><strong>bold inside</strong></span>
  <span class="koboSpan" id="kobo.2.3"> and </span>
  <span class="koboSpan" id="kobo.2.4"><em>italic inside</em></span>
  <span class="koboSpan" id="kobo.2.5"> plus more words here.</span>
</p>
```

## 4. Span map model

```go
type SpanRef struct {
    SpanID      string `json:"span_id"`      // kobo.<block>.<run>
    Selector    string `json:"selector"`     // body-relative XPath of the containing block
    ElementText string `json:"element_text"` // block concatenated descendant text (pre-injection)
    PrefixRunes int    `json:"prefix_runes"` // rune offset of SpanText within ElementText
    SpanText    string `json:"span_text"`    // the span's own text
}
```

`Artifact` fields: `EPUB []byte` (complete zip), `Spans []SpanRef` (document order),
`ChapterHTML []byte` (the XML of `OEBPS/xhtml/ch001.xhtml`), `SpanMapJSON []byte` (JSON
encoding of `Spans`).

> **API note (deviation from the draft spec):** the draft also asked for a method
> `func (a *Artifact) SpanMapJSON() []byte`. Go forbids a field and a method with the same
> name on one type, so the data lives in the field; consumers read `Artifact.SpanMapJSON`
> (or re-marshal `Artifact.Spans`).

## 5. Selector dialect (Readeck)

Selectors are body-relative XPath built from the transformed input tree, evaluated by Readeck
with `./` prepended against its own extracted DOM (stream A §4):

- Every step is either `tag[i]` (1-based position among same-tag *element* siblings) or
  `tag[@id='…']` (preferred when the element has an id).
- Ids are used only when non-empty, unique in the document, whitespace-free, and quotable
  with a single quote style; `"` and `'` in the id select the quote character accordingly;
  ids containing both quote types fall back to the positional form. Duplicate ids always
  fall back to positional.
- No leading `/`, no `//`, no attributes other than the id predicate. Anything else is
  rejected or 500s in Readeck (stream A §4).
- Position counts siblings *of the same tag* (XPath semantics), so `p[3]` may be the 3rd
  paragraph while being, say, the 5th element child.

Because intermediate structure (`section[1]/article[1]/…`) is preserved verbatim, selectors
recorded against a Readeck-served fragment resolve in that same bookmark's served article.
The mapping engine should still **re-verify against the served DOM at write time** (text
matching + `resolve`) — see §8.

Examples from the real fixture (`internal/kepub/testdata/readeck-article.html`):

| span | selector |
|---|---|
| kobo.1.1 | `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']` |
| kobo.2.1 | `section[1]/article[1]/p[@id='uQ.GbYH.p-bravo']` |
| kobo.3.1 | `section[1]/article[1]/p[3]` |
| kobo.5.1 | `section[1]/article[1]/ul[1]/li[1]` |
| kobo.8.1 | `section[1]/article[1]/blockquote[1]` |
| kobo.9.1 | `section[1]/article[1]/p[6]` |

## 6. Offsets

- `PrefixRunes` and all offset math count **runes (Unicode code points)**, matching Readeck's
  own offset semantics (stream A §4: offsets are runes over the element's concatenated
  descendant text, end exclusive).
- `ElementText` is the concatenation of **all descendant text nodes** of the block, exactly as
  Readeck computes element text — including nested `<strong>`/`<em>` text. The recorded
  interior slice `ElementText[PrefixRunes : PrefixRunes+runes(SpanText)]` therefore equals
  `SpanText` even when the block contains nested inline markup (tested).
- Blocks containing nested containers (e.g. `div` with own text plus a nested `p`) record
  `ElementText` over the whole element; a span's start simply lands after the nested text —
  the offset math stays correct because every element's text is a pure concatenation.

## 7. EPUB assembly

Valid EPUB3 zip, in this exact order:

| entry | notes |
|---|---|
| `mimetype` | `application/epub+zip`, **stored (uncompressed), first entry** |
| `META-INF/container.xml` | points at `OEBPS/content.opf` |
| `OEBPS/content.opf` | EPUB3 package: `dc:title`/`dc:creator` from Meta (empty title → `Untitled`, empty author → element omitted), `dc:identifier` = `urn:readeckobo:<sha256(source-url+title)>`, `dc:language=en` (hardcoded, see §9.6), `dcterms:modified`, manifest (ch001, nav, css), spine → ch1 |
| `OEBPS/style.css` | body margins, img/video/svg max-width, `.rd-annotation` background (renders the rewritten Readeck annotation spans on the device), `div.kobostylehacks{display:none}` |
| `OEBPS/xhtml/nav.xhtml` | EPUB3 nav document (`epub:type="toc"`), required by the spec; Kobo ignores it |
| `OEBPS/xhtml/ch001.xhtml` | single chapter, `<html xmlns="http://www.w3.org/1999/xhtml">`, `<meta charset="utf-8"/>`, `<title>`, stylesheet link; body = `<div class="kobostylehacks"></div>` (kepubify convention marker) + the transformed article body |

The chapter is serialized by a small XML writer (not `html.Render`): `html.Render` emits
unclosed void elements and HTML entities such as `&nbsp;` that are not XML-predefined, which
would break `encoding/xml` round-trips. The custom writer self-closes void elements and
escapes text/attributes with the five XML-safe entities only; it is HTML-parsable as well
(validated in tests).

## 8. Mapping contract for downstream consumers

Inputs available to the server mapping engine:

- **Device side** (KoboReader.sqlite, stream B §3, §9): `Bookmark` row with
  `StartContainerPath = span#kobo\.N\.M` (CSS-escaped dots; unescape → `kobo.N.M`),
  `EndContainerPath`, `StartOffset`, `EndOffset` (offsets within the respective span texts),
  `ContentID` → chapter path. Offsets are unverified code-point-wise on real firmware
  (§9.3).
- **Package side**: `SpanRef` per span id: `Selector`, `ElementText`, `PrefixRunes`,
  `SpanText`.

Engine computation for a single-block highlight:

```
startAbs = PrefixRunes(SpanRef(startSpan)) + StartOffset   // runes into ElementText
endAbs   = PrefixRunes(SpanRef(endSpan))   + EndOffset     // exclusive
block    = SpanRef(startSpan).Selector                     // == endSpan's selector when single-block
payload  = { start_selector, start_offset: startAbs, end_selector, end_offset: endAbs, color }
```

then `POST /api/bookmarks/{id}/annotations` (stream A §3.2). The server recomputes `text`
from its own DOM, validates the selector with `./` prepended, and rejects overlaps with 400 —
interpreted as "already highlighted", not a format failure (stream A §4, §8).

**Multi-span, same block** (start and end spans in the same paragraph): the formula above
already covers it since both refs share the block.

**Cross-block highlights** (start span in `kobo.N.M`, end span in `kobo.K.L`, N≠K — user
dragged across paragraphs): Readeck annotations are element-scoped, so a single range cannot
span two blocks. The engine must **split** into per-block annotations:
`[PrefixRunes+StartOffset, runes(ElementText))` for the start block and
`[0, PrefixRunes+EndOffset)` for the end block (plus full intermediate blocks when more than
two are crossed). Offsets within `ElementText` are directly usable as Readeck offsets.

All selector/offset work must run against the **bookmark's served article** (ids are
per-bookmark rewritten, stream A §4); matching is text-based with whitespace-collapsed
comparison because Readeck's extraction may normalize whitespace, and paragraph text in the
kepub equals paragraph text in the served article **by construction** (the transform never
adds or removes text).

## 9. Assumptions & open questions

1. **Id-rewrite stability across fetches.** Readeck rewrites element ids with a per-bookmark
   prefix; whether the prefix is stable across multiple `GET /api/bookmarks/{id}/article`
   calls for the same bookmark is unverified here (stream A §4 observed one prefix per
   bookmark, two bookmarks of the same URL got different prefixes). If prefixes change per
   fetch, id-based selectors must be re-derived each fetch; positional selectors
   (`section[1]/article[1]/p[3]`) are stable regardless of ids and may be the safer choice
   for storage. **Affects whether the engine stores the recorded selector verbatim or
   recomputes.**
2. **Span-vs-sentence granularity.** Runs here split at inline-element boundaries
   (paragraph text without inline markup becomes a single span covering the whole
   paragraph). kepubify instead splits *sentences*. If Kobo's selection snapping is
   span-bound (unverified on real firmware), single-span paragraphs mean a highlight snaps
   to the whole paragraph; kepubify-style sentence segmentation would improve device UX at
   the cost of a more complex text splitter (abbreviations, decimals, spacing) and more
   spans to map. Open.
3. **Kobo offset units.** `StartOffset`/`EndOffset` semantics on real firmware (code points
   vs UTF-16 units, whitespace handling) are UNVERIFIED (stream B §11.2). The kepub side
   defines offsets as runes and keeps span text verbatim (no normalization), which is the
   self-consistent choice; a fixture device dump is needed to confirm Nickel agrees.
4. **Uncovered text types.** `pre`/`code` blocks and image-only paragraphs receive no spans
   → not highlightable on device. Articles heavy in code blocks (docs, tutorials) would be
   partially non-highlightable; adding `pre` to the container set is a one-line change but
   was deliberately left out to keep the container set as specified. Bidi/`<ruby>` text,
   `<svg>` content and other exotic inline content are treated as opaque inline runs.
5. **rd-annotation chrome.** RESOLVED — Readeck's `rd-*` wrappers are no
   longer dropped (the transform used to unwrap them, so the highlighted run
   became plain text with no on-device trace). `internal/kepub` now
   REWRITES every element whose tag starts with `rd-` into a styled
   `<span class="rd-…">` element: the tag becomes `span`, the original tag
   name becomes the class (`rd-annotation`, `rd-note`, …), and the remaining
   attributes (`id`, `data-annotation-*`, `title`) plus children stay in
   place. Pre-existing Readeck highlights therefore render on the device
   through the `.rd-annotation` stylesheet rule (fix landed; on-device
   rendering of the styled spans still needs the real-device checklist).
   Annotation notes (`title` on the empty note-marker siblings) are still not
   carried into the kepub — Readeck already stores them server-side.
6. **`dc:language` is hardcoded `en`.** `Meta` carries no language field; Readeck bookmarks
   expose `lang`, so threading it through `Meta` later is trivial. EPUB3 requires the
   element; a wrong value may affect hyphenation in CSS-enabled readers (Kobo ignores it).
7. **HTML-in-XHTML text.** Script/style raw text is escaped like any text (XML-valid but a
   `<`-containing script would not survive); article content should not contain scripts.
   XML control characters (U+0000–U+0008 etc.) are not stripped; degenerate inputs may
   produce XML that `encoding/xml` rejects. Both are "garbage in, garbage out" edges.
8. **XML serialization details.** Comments are sanitized (`--` → `- -`) to stay
   XML-valid; `>` is always escaped so `]]>` cannot appear in text.

## 10. Quality gates & test coverage

- `gofmt -l internal/kepub` → clean
- `go vet ./internal/kepub` → clean
- `go test ./internal/kepub` → pass
- `go build ./...` from repo root → pass

Tests (`kepub_test.go`) reimplement the walk/selector/prefix logic independently and verify,
for every fixture:

- normalized body text is preserved exactly (input vs generated chapter);
- block/run numbering (`kobo.<block>.<run>`) is contiguous, doc-ordered, unique;
- `PrefixRunes`/`ElementText`/`SpanText` match independent recomputation, and
  `ElementText[PrefixRunes:…]` slices to `SpanText`;
- selectors resolve (with a small dialect resolver) to the exact block whose text was
  recorded — for the whole span map of every fixture;
- spans tile each container's text exactly; no nested spans; no uncovered non-whitespace
  text; no `rd-*` element tag survives (each is rewritten to `<span class="rd-…">`);
- the chapter, nav, container.xml and OPF parse with `encoding/xml`; the zip layout is
  exact (mimetype stored first, 6 entries, manifest/spine wiring, koboSpan ids in the
  chapter == span map).
- the real Readeck fixtures: 14 spans / 10 blocks with the exact selectors/id/prefix
  literals listed in §5; the annotations variant re-segments the FOXTROT block (15 spans vs
  14 — the trailing `.` becomes its own run because the annotation ends before the period)
  but keeps the same block set and per-block text coverage (`TestRealFixtureAnnotationTransparency`).

## 11. Regenerating the fixtures

`internal/kepub/testdata/readeck-article.html` and
`readeck-article-with-annotations.html` are captured from a live Readeck 0.23.2 instance
(`/tmp/readeck-spike`, see `docs/research/readeck-annotations-api.md` §1 for setup):

```sh
# plain article fragment (per-bookmark rewritten ids):
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8017/api/bookmarks/U9kyUWpFYbYwaXoK8WqLU/article \
  > internal/kepub/testdata/readeck-article.html

# same fragment after creating annotations (rd-annotation wrappers present):
#   (annotations were POSTed from /tmp/readeck-spike/req.sh runs; any set works)
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8017/api/bookmarks/U9kyUWpFYbYwaXoK8WqLU/article \
  > internal/kepub/testdata/readeck-article-with-annotations.html
```

The files must start with `<section><article>` and use Readeck's rewritten id style; the
paragraph texts are the "ALPHA/BRAVO/…/GOLF" corpus the spike assertions and the API research
refer to, so keep the corpus text stable or update the literal assertions in the tests.