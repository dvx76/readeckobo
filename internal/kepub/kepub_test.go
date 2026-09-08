package kepub_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"readeckobo/internal/kepub"
)

var testMeta = kepub.Meta{
	Title:     "Test Article",
	Author:    "Ada Example",
	SourceURL: "https://example.com/article",
}

// ---------------------------------------------------------------------------
// Independent HTML-side helpers (deliberately reimplemented, not sharing
// production code paths, so tests verify semantics rather than implementation)
// ---------------------------------------------------------------------------

// blockTags mirrors the production container set.
var blockTags = map[string]bool{
	"p": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true,
	"dd":         true, "dt": true,
	"td": true, "th": true,
	"figcaption": true, "div": true,
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == strings.ToLower(key) {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, word string) bool {
	for _, f := range strings.Fields(attrOf(n, "class")) {
		if f == word {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// normalizedBodyText parses src as HTML and returns the whitespace-collapsed
// text of its <body>. Used to compare document text before/after the
// transform, where only insignificant inter-block whitespace may differ.
func normalizedBodyText(t *testing.T, src []byte) string {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var body *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if body != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		t.Fatal("no body in parse")
	}
	return strings.Join(strings.Fields(textOf(body)), " ")
}

// collectBlocks returns block elements with at least one direct koboSpan
// child, in document order.
func collectBlocks(n *html.Node, out *[]*html.Node) {
	if n.Type != html.ElementNode {
		return
	}
	if blockTags[n.Data] {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && hasClass(c, "koboSpan") {
				*out = append(*out, n)
				break
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectBlocks(c, out)
	}
}

// resolveStep selects elements of tag among candidates; either by position
// (1-based among same-tag siblings) or by id predicate.
func resolveStep(cands []*html.Node, step string) []*html.Node {
	inner := strings.TrimSuffix(step, "]")
	open := strings.Index(inner, "[")
	if open < 0 {
		return nil
	}
	tag := inner[:open]
	expr := inner[open+1:]
	var out []*html.Node
	if strings.HasPrefix(expr, "@id=") {
		q := expr[len("@id="):]
		if len(q) < 2 || (q[0] != '\'' && q[0] != '"') || q[len(q)-1] != q[0] {
			return nil
		}
		want := q[1 : len(q)-1]
		for _, c := range cands {
			for e := c.FirstChild; e != nil; e = e.NextSibling {
				if e.Type == html.ElementNode && strings.EqualFold(e.Data, tag) && attrOf(e, "id") == want {
					out = append(out, e)
				}
			}
		}
		return out
	}
	pos, err := strconv.Atoi(expr)
	if err != nil {
		return nil
	}
	for _, c := range cands {
		n := 0
		for e := c.FirstChild; e != nil; e = e.NextSibling {
			if e.Type != html.ElementNode || !strings.EqualFold(e.Data, tag) {
				continue
			}
			n++
			if n == pos {
				out = append(out, e)
			}
		}
	}
	return out
}

// resolveSelector resolves a Readeck-dialect selector against body.
func resolveSelector(t *testing.T, body *html.Node, sel string) *html.Node {
	t.Helper()
	if sel == "" || strings.HasPrefix(sel, "/") || strings.Contains(sel, "//") {
		t.Fatalf("selector %q is not body-relative Readeck dialect", sel)
	}
	cands := []*html.Node{body}
	for _, step := range strings.Split(sel, "/") {
		cands = resolveStep(cands, step)
		if len(cands) == 0 {
			t.Fatalf("selector %q does not resolve", sel)
		}
	}
	if len(cands) != 1 {
		t.Fatalf("selector %q resolves to %d nodes", sel, len(cands))
	}
	return cands[0]
}

// checkStructure independently verifies, for a produced chapter:
//   - every koboSpan is a direct child of a block container, no span nests in
//     another span, and all span ids are unique;
//   - per block: span ids follow kobo.<block>.<run> with block/run counters in
//     document order; PrefixRunes/ElementText/SpanText/Selector recorded in
//     the span map match an independent recomputation from the serialized
//     chapter, and spans tile the block text exactly (every character of the
//     block's own text is in exactly one span; text of nested block
//     containers is accounted separately);
//   - selectors resolve against the chapter <body> to the very block whose
//     text was recorded.
func checkStructure(t *testing.T, chapter []byte, spans []kepub.SpanRef) {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(chapter))
	if err != nil {
		t.Fatalf("reparse chapter: %v", err)
	}
	var body *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if body != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		t.Fatal("chapter has no body")
	}

	byID := map[string]kepub.SpanRef{}
	for _, s := range spans {
		byID[s.SpanID] = s
	}

	// Global pass: no nested spans, spans only directly under block
	// containers, ids unique, and every non-whitespace text node lives under
	// a span-bearing block container (spans tile each processed container).
	spanBearing := func(blk *html.Node) bool {
		if blk == nil || blk.Type != html.ElementNode || !blockTags[blk.Data] {
			return false
		}
		for c := blk.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && hasClass(c, "koboSpan") {
				return true
			}
		}
		return false
	}
	seenIDs := map[string]bool{}
	spanCount := 0
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, insideSpan bool) {
		if n.Type == html.ElementNode {
			if hasClass(n, "koboSpan") {
				spanCount++
				if insideSpan {
					t.Errorf("koboSpan %s nested inside another koboSpan", attrOf(n, "id"))
				}
				if n.Parent == nil || !blockTags[n.Parent.Data] {
					t.Errorf("koboSpan %s parent %q is not a block container", attrOf(n, "id"), n.Parent.Data)
				}
				if seenIDs[attrOf(n, "id")] {
					t.Errorf("duplicate koboSpan id %s", attrOf(n, "id"))
				}
				seenIDs[attrOf(n, "id")] = true
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, insideSpan || hasClass(n, "koboSpan"))
			}
			return
		}
		if n.Type == html.TextNode && strings.IndexFunc(n.Data, func(r rune) bool { return !unicode.IsSpace(r) }) >= 0 {
			covered := false
			for a := n.Parent; a != nil && !covered; a = a.Parent {
				covered = spanBearing(a)
			}
			if !covered {
				t.Errorf("uncovered non-whitespace text %q", n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, insideSpan)
		}
	}
	walk(body, false)

	if spanCount != len(spans) {
		t.Fatalf("chapter has %d koboSpan elements but span map has %d", spanCount, len(spans))
	}

	// Per-block verification.
	var blocks []*html.Node
	collectBlocks(body, &blocks)
	for bi, blk := range blocks {
		elementText := textOf(blk)
		if _, ok := byID[fmt.Sprintf("kobo.%d.1", bi+1)]; !ok {
			t.Errorf("block %d (%s) does not start at kobo.%d.1", bi+1, blk.Data, bi+1)
		}
		acc := 0
		seg := 0
		for c := blk.FirstChild; c != nil; c = c.NextSibling {
			txt := textOf(c)
			if c.Type == html.ElementNode && hasClass(c, "koboSpan") {
				seg++
				wantID := fmt.Sprintf("kobo.%d.%d", bi+1, seg)
				ref, ok := byID[wantID]
				if !ok {
					t.Fatalf("span id %s missing from map", wantID)
				}
				if ref.SpanText != txt {
					t.Errorf("%s: recorded SpanText %q != actual %q", wantID, ref.SpanText, txt)
				}
				if ref.PrefixRunes != acc {
					t.Errorf("%s: recorded PrefixRunes %d != recomputed %d", wantID, ref.PrefixRunes, acc)
				}
				if ref.ElementText != elementText {
					t.Errorf("%s: recorded ElementText %q != actual %q", wantID, ref.ElementText, elementText)
				}
				if got := resolveSelector(t, body, ref.Selector); got != blk {
					t.Errorf("%s: selector %q does not resolve to its block (%s)", wantID, ref.Selector, blk.Data)
				}
				if got := runeSlice(elementText, ref.PrefixRunes, ref.PrefixRunes+utf8.RuneCountInString(txt)); got != txt {
					t.Errorf("%s: ElementText slice at PrefixRunes is %q, want %q", wantID, got, txt)
				}
				acc += utf8.RuneCountInString(txt)
				continue
			}
			acc += utf8.RuneCountInString(txt)
		}
		if total := utf8.RuneCountInString(elementText); acc != total {
			t.Errorf("block %s: covered runes %d != element text runes %d (gap or overlap)", blk.Data, acc, total)
		}
	}
	if len(blocks) > 0 {
		if _, ok := byID[fmt.Sprintf("kobo.%d.1", len(blocks))]; !ok {
			t.Errorf("block numbering does not end at kobo.%d.1", len(blocks))
		}
	}
}

// runeSlice returns the substring of s covering runes [start, end).
func runeSlice(s string, start, end int) string {
	if start < 0 || end < start || end > utf8.RuneCountInString(s) {
		return ""
	}
	runes := []rune(s)
	return string(runes[start:end])
}

func assertXMLWellFormed(t *testing.T, data []byte, what string) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				return
			}
			t.Fatalf("%s is not well-formed XML: %v", what, err)
		}
	}
}

// assertNoRDElements asserts the transform converted every rd-* wrapper into
// a regular span (no element tag may still start with "rd-").
func assertNoRDElements(t *testing.T, chapter []byte) {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(chapter))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.HasPrefix(strings.ToLower(n.Data), "rd-") {
			t.Errorf("rd-* element %q survived the transform", n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// koboIDsFromXML returns every id attribute starting with "kobo." found in an
// XML document, in document order.
func koboIDsFromXML(t *testing.T, data []byte) []string {
	t.Helper()
	var ids []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tokenize: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			for _, a := range se.Attr {
				if strings.HasPrefix(a.Value, "kobo.") {
					ids = append(ids, a.Value)
				}
			}
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// Synthetic fixture tests
// ---------------------------------------------------------------------------

func TestBuildSynthetic(t *testing.T) {
	cases := []struct {
		name       string
		html       string
		wantBlocks int
		wantSpans  int
	}{
		{
			name:       "plain paragraph",
			html:       `<p>Hello world.</p>`,
			wantBlocks: 1, wantSpans: 1,
		},
		{
			name: "nested inline markup",
			html: `<p>BRAVO <strong>bold <em>deep</em> tail</strong> and <em>italic</em> more.</p>`,
			// runs: "BRAVO " | <strong>…</strong> | " and " | <em>italic</em> | " more."
			wantBlocks: 1, wantSpans: 5,
		},
		{
			name:       "html entities",
			html:       `<p>fish &amp; chips &#x1F389; &lt;tag&gt; &quot;quoted&quot; &nbsp;nbsp &apos;apos&apos;</p>`,
			wantBlocks: 1, wantSpans: 1,
		},
		{
			name:       "unicode and emoji",
			html:       `<p>Ünïcødé — 日本語テスト — emoji: 🎉😀</p>`,
			wantBlocks: 1, wantSpans: 1,
		},
		{
			name: "multiple blocks",
			html: `<p>One.</p><p>Two.</p><ul><li>item 1</li><li>item 2</li></ul>` +
				`<blockquote>quoted text</blockquote><h2>Heading</h2><h3>Subheading</h3>`,
			wantBlocks: 7, wantSpans: 7,
		},
		{
			name: "nested div with own text",
			html: `<div id="outer">intro text<div><p>inner paragraph</p></div>outro text</div>`,
			// outer div: 2 runs around the nested div; inner p: 1 run.
			wantBlocks: 2, wantSpans: 3,
		},
		{
			name: "rd-annotation rewrite",
			html: `<p><rd-annotation id="annotation-x" data-annotation-color="yellow">Highlighted sentence one.</rd-annotation>` +
				`<rd-annotation data-annotation-note="" title="a note"></rd-annotation> tail text</p>`,
			// The wrappers become inline spans: the annotated run and the
			// tail text are separate runs around the empty note marker.
			wantBlocks: 1, wantSpans: 2,
		},
		{
			name: "comment splits text runs",
			html: `<p>alpha<!--mid-->omega</p>`,
			// two text runs, the comment is not wrapped and carries no text.
			wantBlocks: 1, wantSpans: 2,
		},
		{
			name: "br between text runs",
			html: `<p>first<br>second</p>`,
			// br carries no text and is not wrapped; text runs tile the text.
			wantBlocks: 1, wantSpans: 2,
		},
		{
			name:       "empty and whitespace-only blocks skipped",
			html:       `<p></p><p>   </p><div></div><p>real text</p>`,
			wantBlocks: 1, wantSpans: 1,
		},
		{
			name: "dl and table cells",
			html: `<dl><dt>term</dt><dd>definition</dd></dl>` +
				`<table><tr><th>Header</th></tr><tr><td>cell data</td></tr></table>`,
			wantBlocks: 4, wantSpans: 4,
		},
		{
			name:       "figcaption and heading",
			html:       `<figure><figcaption>A caption</figcaption></figure><h1>Title One</h1>`,
			wantBlocks: 2, wantSpans: 2,
		},
		{
			name:       "section article wrapper",
			html:       `<section><article><p id="a">T</p></article></section>`,
			wantBlocks: 1, wantSpans: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := kepub.Build(tc.html, testMeta)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if len(a.Spans) != tc.wantSpans {
				t.Fatalf("got %d spans, want %d", len(a.Spans), tc.wantSpans)
			}
			if got, want := normalizedBodyText(t, []byte(tc.html)), normalizedBodyText(t, a.ChapterHTML); got != want {
				t.Errorf("normalized text changed:\n before: %q\n after:  %q", got, want)
			}
			checkStructure(t, a.ChapterHTML, a.Spans)
			assertNoRDElements(t, a.ChapterHTML)
			assertXMLWellFormed(t, a.ChapterHTML, "chapter")
			assertSpanMapJSON(t, a)
		})
	}
}

// assertSpanMapJSON verifies the SpanMapJSON payload round-trips to Spans and
// uses the documented JSON keys.
func assertSpanMapJSON(t *testing.T, a *kepub.Artifact) {
	t.Helper()
	var got []kepub.SpanRef
	if err := json.Unmarshal(a.SpanMapJSON, &got); err != nil {
		t.Fatalf("SpanMapJSON does not parse: %v", err)
	}
	if !reflect.DeepEqual(got, a.Spans) {
		t.Errorf("SpanMapJSON round-trip mismatch:\n got  %+v\n want %+v", got, a.Spans)
	}
	raw := string(a.SpanMapJSON)
	for _, key := range []string{`"span_id"`, `"selector"`, `"element_text"`, `"prefix_runes"`, `"span_text"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("SpanMapJSON missing key %s", key)
		}
	}
}

func TestBuildEmptyDocument(t *testing.T) {
	a, err := kepub.Build("", testMeta)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(a.Spans) != 0 {
		t.Fatalf("expected no spans, got %d", len(a.Spans))
	}
	if string(a.SpanMapJSON) != "[]" {
		t.Errorf("empty span map JSON = %q, want []", a.SpanMapJSON)
	}
	checkStructure(t, a.ChapterHTML, a.Spans)
	assertXMLWellFormed(t, a.ChapterHTML, "chapter")
}

func TestSelectorDialect(t *testing.T) {
	// findSpanText returns the ref of the span whose text equals txt.
	findText := func(spans []kepub.SpanRef, txt string) kepub.SpanRef {
		for _, s := range spans {
			if s.SpanText == txt {
				return s
			}
		}
		t.Fatalf("no span with text %q in %+v", txt, spans)
		return kepub.SpanRef{}
	}
	cases := []struct {
		name string
		html string
		text string // span text whose block's selector is asserted
		sel  string // expected selector for that block
	}{
		{"id step", `<section><article><p id="uQ.GbYH.p-alpha">ALPHA text.</p></article></section>`,
			"ALPHA text.", `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']`},
		{"positional step", `<section><article><p>a</p><p>b</p><p>c</p></article></section>`,
			"c", `section[1]/article[1]/p[3]`},
		{"list item", `<section><article><ul><li>one</li><li>two</li></ul></article></section>`,
			"two", `section[1]/article[1]/ul[1]/li[2]`},
		{"duplicate ids fall back to positional", `<section><article><p id="dup">a</p><p id="dup">b</p></article></section>`,
			"b", `section[1]/article[1]/p[2]`},
		{"double-quoted id", `<section><article><p id="it's">text</p></article></section>`,
			"text", `section[1]/article[1]/p[@id="it's"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := kepub.Build(tc.html, testMeta)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			ref := findText(a.Spans, tc.text)
			if ref.Selector != tc.sel {
				t.Errorf("selector = %q, want %q", ref.Selector, tc.sel)
			}
			checkStructure(t, a.ChapterHTML, a.Spans)
		})
	}
}

// ---------------------------------------------------------------------------
// Real Readeck fixture tests
// ---------------------------------------------------------------------------

func TestRealReadeckArticles(t *testing.T) {
	for _, file := range []string{
		"testdata/readeck-article.html",
		"testdata/readeck-article-with-annotations.html",
	} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			a, err := kepub.Build(string(raw), testMeta)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			checkStructure(t, a.ChapterHTML, a.Spans)
			assertXMLWellFormed(t, a.ChapterHTML, "chapter")
			assertNoRDElements(t, a.ChapterHTML)
			assertSpanMapJSON(t, a)

			if got, want := normalizedBodyText(t, raw), normalizedBodyText(t, a.ChapterHTML); got != want {
				t.Errorf("normalized text changed:\n before: %q\n after:  %q", got, want)
			}
			wantSpans := 14
			if file == "testdata/readeck-article-with-annotations.html" {
				// FOXTROT's annotation ends before the final period, so its
				// block gains a second run for the trailing ".".
				wantSpans = 15
			}
			if len(a.Spans) != wantSpans {
				t.Errorf("got %d spans, want %d", len(a.Spans), wantSpans)
			}
			byID := map[string]kepub.SpanRef{}
			for _, s := range a.Spans {
				byID[s.SpanID] = s
			}
			// Exact expectations for a few well-known rows.
			alpha := byID["kobo.1.1"]
			if alpha.Selector != `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']` {
				t.Errorf("alpha selector = %q", alpha.Selector)
			}
			if alpha.ElementText != "ALPHA one two three four five six seven eight nine ten." ||
				alpha.PrefixRunes != 0 || alpha.SpanText != alpha.ElementText {
				t.Errorf("alpha ref wrong: %+v", alpha)
			}
			for i, want := range []struct {
				text   string
				prefix int
			}{
				{"BRAVO ", 0},
				{"bold inside", 6},
				{" and ", 17},
				{"italic inside", 22},
				{" plus more words here.", 35},
			} {
				ref := byID[fmt.Sprintf("kobo.2.%d", i+1)]
				if ref.SpanText != want.text || ref.PrefixRunes != want.prefix {
					t.Errorf("bravo kobo.2.%d = %+v, want text %q prefix %d", i+1, ref, want.text, want.prefix)
				}
			}
			if ref := byID["kobo.2.1"]; ref.Selector != `section[1]/article[1]/p[@id='uQ.GbYH.p-bravo']` {
				t.Errorf("bravo selector = %q", ref.Selector)
			}
			if ref := byID["kobo.3.1"]; ref.Selector != "section[1]/article[1]/p[3]" {
				t.Errorf("charlie selector = %q", ref.Selector)
			}
			if ref := byID["kobo.5.1"]; ref.Selector != "section[1]/article[1]/ul[1]/li[1]" ||
				ref.ElementText != "LIST item one alpha" {
				t.Errorf("li1 ref wrong: %+v", ref)
			}
			if ref := byID["kobo.6.1"]; ref.Selector != "section[1]/article[1]/ul[1]/li[2]" {
				t.Errorf("li2 selector = %q", ref.Selector)
			}
			if ref := byID["kobo.7.1"]; ref.Selector != `section[1]/article[1]/p[@id='uQ.GbYH.p-echo']` {
				t.Errorf("echo selector = %q", ref.Selector)
			}
			if ref := byID["kobo.8.1"]; ref.Selector != "section[1]/article[1]/blockquote[1]" {
				t.Errorf("blockquote selector = %q", ref.Selector)
			}
			if ref := byID["kobo.9.1"]; ref.Selector != "section[1]/article[1]/p[6]" ||
				ref.ElementText != "FOXTROT golf hotel india juliet kilo lima mike november oscar." {
				t.Errorf("foxtrot ref wrong: %+v", ref)
			}
			if file == "testdata/readeck-article-with-annotations.html" {
				if ref := byID["kobo.9.2"]; ref.Selector != "section[1]/article[1]/p[6]" ||
					ref.SpanText != "." || ref.PrefixRunes != 61 {
					t.Errorf("foxtrot trailing-dot ref wrong: %+v", ref)
				}
			}
			if ref := byID["kobo.10.1"]; ref.Selector != "section[1]/article[1]/p[7]" {
				t.Errorf("golf selector = %q", ref.Selector)
			}
		})
	}
}

// The annotations fixture is the plain fixture with rd-annotation wrappers
// around highlight text (plus empty note-marker siblings). The wrappers
// become styled spans, which re-segments the koboSpan runs around annotated
// text, so the span maps may differ in segmentation — but the per-block text
// coverage and the selector set must be identical.
func TestRealFixtureAnnotationTransparency(t *testing.T) {
	plain, err := os.ReadFile("testdata/readeck-article.html")
	if err != nil {
		t.Fatal(err)
	}
	annotated, err := os.ReadFile("testdata/readeck-article-with-annotations.html")
	if err != nil {
		t.Fatal(err)
	}
	aPlain, err := kepub.Build(string(plain), testMeta)
	if err != nil {
		t.Fatal(err)
	}
	aAnnotated, err := kepub.Build(string(annotated), testMeta)
	if err != nil {
		t.Fatal(err)
	}

	// Per-selector block coverage: the concatenated span text of every block
	// must be identical across both builds, and both must see the same blocks.
	coverage := func(spans []kepub.SpanRef) map[string]string {
		m := map[string]string{}
		for _, s := range spans {
			m[s.Selector] += s.SpanText
		}
		return m
	}
	cp, ca := coverage(aPlain.Spans), coverage(aAnnotated.Spans)
	if len(cp) != len(ca) {
		t.Fatalf("block set differs: plain %d blocks, annotated %d blocks", len(cp), len(ca))
	}
	for sel, text := range cp {
		if ca[sel] != text {
			t.Errorf("block %s coverage differs:\n plain: %q\n annotated: %q", sel, text, ca[sel])
		}
	}

	if got, want := normalizedBodyText(t, aPlain.ChapterHTML), normalizedBodyText(t, aAnnotated.ChapterHTML); got != want {
		t.Errorf("normalized chapter text differs:\n plain: %q\n annotated: %q", got, want)
	}
}

// TestRDAwareAnnotationRewrite asserts the rd-* wrappers come out as styled
// spans: no rd-* tag survives, the class carries the original tag name, the
// original attributes are preserved, and the document text is unchanged.
func TestRDAwareAnnotationRewrite(t *testing.T) {
	in := `<p><rd-annotation id="annotation-x" data-annotation-color="yellow">Highlighted sentence one.</rd-annotation>` +
		`<rd-annotation data-annotation-note="" title="a note"></rd-annotation> tail text</p>`
	a, err := kepub.Build(in, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(a.ChapterHTML))
	if err != nil {
		t.Fatal(err)
	}
	var sawStyled int
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if strings.HasPrefix(strings.ToLower(n.Data), "rd-") {
				t.Errorf("rd-* element %q survived the transform", n.Data)
			}
			if n.Data == "span" {
				for _, at := range n.Attr {
					if at.Key == "class" && at.Val == "rd-annotation" {
						sawStyled++
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if sawStyled != 2 {
		t.Errorf("found %d span.rd-annotation elements, want 2", sawStyled)
	}
	if got, want := normalizedBodyText(t, a.ChapterHTML), "Highlighted sentence one. tail text"; got != want {
		t.Errorf("normalized chapter text = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// EPUB validity tests
// ---------------------------------------------------------------------------

func TestEPUBStructure(t *testing.T) {
	raw, err := os.ReadFile("testdata/readeck-article-with-annotations.html")
	if err != nil {
		t.Fatal(err)
	}
	a, err := kepub.Build(string(raw), testMeta)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(a.EPUB), int64(len(a.EPUB)))
	if err != nil {
		t.Fatalf("EPUB is not a zip: %v", err)
	}
	if len(zr.File) != 6 {
		t.Fatalf("zip has %d entries, want 6", len(zr.File))
	}
	first := zr.File[0]
	if first.Name != "mimetype" {
		t.Fatalf("first zip entry is %q, want mimetype", first.Name)
	}
	if first.Method != zip.Store {
		t.Errorf("mimetype entry must be stored, method = %d", first.Method)
	}

	read := func(name string) []byte {
		t.Helper()
		for _, f := range zr.File {
			if f.Name == name {
				rc, err := f.Open()
				if err != nil {
					t.Fatalf("open %s: %v", name, err)
				}
				defer rc.Close()
				data, err := io.ReadAll(rc)
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				return data
			}
		}
		t.Fatalf("zip missing %s", name)
		return nil
	}

	if got := string(read("mimetype")); got != "application/epub+zip" {
		t.Errorf("mimetype = %q", got)
	}

	// container.xml → OPF path.
	var container struct {
		Rootfiles []struct {
			Rootfile []struct {
				FullPath  string `xml:"full-path,attr"`
				MediaType string `xml:"media-type,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	containerData := read("META-INF/container.xml")
	if err := xml.Unmarshal(containerData, &container); err != nil {
		t.Fatalf("container.xml: %v", err)
	}
	opfPath := container.Rootfiles[0].Rootfile[0].FullPath
	if opfPath != "OEBPS/content.opf" {
		t.Fatalf("container rootfile = %q", opfPath)
	}
	opfData := read(opfPath)
	assertXMLWellFormed(t, opfData, "content.opf")

	// OPF manifest/spine wiring.
	var manifestItems = map[string]map[string]string{} // id → attrs
	var spineRefs []string
	dec := xml.NewDecoder(bytes.NewReader(opfData))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("opf tokenize: %v", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		attrs := map[string]string{}
		for _, at := range se.Attr {
			attrs[at.Name.Local] = at.Value
		}
		switch se.Name.Local {
		case "item":
			manifestItems[attrs["id"]] = attrs
		case "itemref":
			spineRefs = append(spineRefs, attrs["idref"])
		}
	}
	if attrs := manifestItems["ch1"]; attrs["href"] != "xhtml/ch001.xhtml" || attrs["media-type"] != "application/xhtml+xml" {
		t.Errorf("ch1 manifest item = %v", attrs)
	}
	if attrs := manifestItems["nav"]; attrs["href"] != "xhtml/nav.xhtml" || attrs["media-type"] != "application/xhtml+xml" || attrs["properties"] != "nav" {
		t.Errorf("nav manifest item = %v", attrs)
	}
	if attrs := manifestItems["css"]; attrs["href"] != "style.css" || attrs["media-type"] != "text/css" {
		t.Errorf("css manifest item = %v", attrs)
	}
	if len(spineRefs) != 1 || spineRefs[0] != "ch1" {
		t.Errorf("spine itemrefs = %v, want [ch1]", spineRefs)
	}
	if !strings.Contains(string(opfData), "<dc:title>Test Article</dc:title>") ||
		!strings.Contains(string(opfData), "<dc:creator>Ada Example</dc:creator>") {
		t.Errorf("OPF metadata missing title/creator:\n%s", opfData)
	}

	// Chapter: well-formed XML, koboSpan ids match the span map exactly.
	chapter := read("OEBPS/xhtml/ch001.xhtml")
	assertXMLWellFormed(t, chapter, "ch001.xhtml")
	ids := koboIDsFromXML(t, chapter)
	if len(ids) != len(a.Spans) {
		t.Fatalf("chapter has %d koboSpan ids, span map has %d", len(ids), len(a.Spans))
	}
	wantIDs := map[string]bool{}
	for _, s := range a.Spans {
		wantIDs[s.SpanID] = true
	}
	for _, id := range ids {
		if !wantIDs[id] {
			t.Errorf("chapter id %s not in span map", id)
		}
	}

	// Supporting files parse as XML / exist.
	assertXMLWellFormed(t, read("OEBPS/xhtml/nav.xhtml"), "nav.xhtml")
	if css := read("OEBPS/style.css"); len(css) == 0 {
		t.Error("style.css is empty")
	}
}
