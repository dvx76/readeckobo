// Package kepub converts article HTML into a Kobo-kepub-compatible EPUB whose
// highlights can be position-mapped back into Readeck annotations.
//
// # What the package does
//
// Build parses article HTML (expected shape: a Readeck article fragment
// `<section><article>…</article></section>`, or HTML with the same block
// structure), rewrites Readeck's `rd-*` annotation wrapper elements into
// styled `<span class="rd-…">` elements (inner content and text are unchanged,
// so highlights already made in Readeck render on the device via the
// .rd-annotation CSS rule), and wraps every text run of every block-level
// element in a
// `<span class="koboSpan" id="kobo.<block>.<run>">` element — the span
// convention Kobo's reader uses to anchor highlights (see the project's
// research notes in docs/research/).
//
// # Invariants
//
//   - Spans carry no text: the normalized text content of the document is
//     byte-identical before and after injection.
//   - Within every processed block element, each text character is covered by
//     exactly one span; spans are siblings and appear in document order.
//   - Span ids are unique and follow the kepubify numbering scheme
//     `kobo.<block>.<run>` (1-based counters in document order).
//
// For every injected span the returned Artifact carries a SpanRef recording
// the body-relative XPath selector of the containing block in Readeck's
// selector dialect (no leading `/`, no `//`, `tag[i]` or `tag[@id='…']`
// steps), the block's concatenated descendant text, the rune offset of the
// span's text within that text, and the span's own text. A downstream
// mapping engine can combine that map with Kobo Bookmark rows
// (`span#kobo\.N\.M` container paths + StartOffset/EndOffset) to compute
// Readeck annotation ranges. See docs/research/kepub-generation.md.
package kepub

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Meta carries the bibliographic metadata embedded in the generated EPUB.
type Meta struct {
	Title     string `json:"title"`
	Author    string `json:"author"`
	SourceURL string `json:"source_url"`
}

// SpanRef is the position-map entry for one injected koboSpan. The fields
// describe the *containing block element* (a block-level element of the input
// article, e.g. `p`, `li`, `blockquote`) plus the exact position of this
// span's text inside that block:
//
//	ElementText  = concatenated text of all descendant text nodes of the block
//	               (as it reads before span injection — spans add no text),
//	PrefixRunes  = rune offset of the span's own text start inside ElementText,
//	SpanText     = the span's own (descendant) text,
//	Selector     = body-relative XPath locating the block in Readeck's dialect.
//
// A Kobo highlight row referencing this span (StartContainerPath =
// `span#kobo\.<block>\.<run>`, offset within the span's text) therefore maps
// to the absolute rune range [PrefixRunes+StartOffset, PrefixRunes+EndOffset)
// inside the block's ElementText.
type SpanRef struct {
	SpanID      string `json:"span_id"`      // kobo.<block>.<run>
	Selector    string `json:"selector"`     // e.g. section[1]/article[1]/p[3]
	ElementText string `json:"element_text"` // block text (concatenated descendants)
	PrefixRunes int    `json:"prefix_runes"` // rune offset of SpanText within ElementText
	SpanText    string `json:"span_text"`    // this span's text
}

// Artifact is the result of Build.
type Artifact struct {
	EPUB        []byte    // complete .kepub.epub (zip)
	Spans       []SpanRef // span map, in document order
	ChapterHTML []byte    // OEBPS/xhtml/ch001.xhtml content
	SpanMapJSON []byte    // JSON encoding of Spans
}

// NOTE: an earlier API draft also specified a method SpanMapJSON() []byte.
// Go forbids a field and a method sharing a name on one type, so the data
// lives in the field above; callers read Artifact.SpanMapJSON or re-marshal
// Artifact.Spans.

// Build turns articleHTML into a kepub Artifact.
//
// articleHTML is parsed leniently as HTML (entities decoded, tag soup
// tolerated). Its body content is expected to be the Readeck article
// fragment (`<section><article>…`) so that recorded selectors resolve against
// the article DOM served by Readeck; the transform never restructures the
// block hierarchy, so any input with the same block structure works too.
func Build(articleHTML string, meta Meta) (*Artifact, error) {
	doc, err := html.Parse(strings.NewReader(articleHTML))
	if err != nil {
		return nil, fmt.Errorf("kepub: parse article HTML: %w", err)
	}
	body := findBody(doc)
	if body == nil {
		return nil, fmt.Errorf("kepub: article HTML contains no body element")
	}

	spans := transformBody(body)
	chapterXML := renderXHTML(buildChapterTree(meta, body))

	epubData, err := assembleEPUB(chapterXML, meta)
	if err != nil {
		return nil, fmt.Errorf("kepub: assemble EPUB: %w", err)
	}
	spanMap, err := json.Marshal(spans)
	if err != nil {
		return nil, fmt.Errorf("kepub: marshal span map: %w", err)
	}
	return &Artifact{
		EPUB:        epubData,
		Spans:       spans,
		ChapterHTML: chapterXML,
		SpanMapJSON: spanMap,
	}, nil
}

// ---------------------------------------------------------------------------
// HTML transform
// ---------------------------------------------------------------------------

// blockContainers lists the block-level elements whose text runs are wrapped
// in koboSpan elements. The set is a deliberate choice: it covers the
// paragraph-like containers Readeck's extraction produces inside
// section/article (p, headings, list items, blockquote, description-list and
// table cells, figcaption) plus plain divs carrying text directly (Readeck
// hoists most divs away, but arbitrary article HTML may contain them).
// Notable exclusions, documented in docs/research/kepub-generation.md:
// section/article/ul/ol/table/tr wrappers themselves (they normally carry no
// direct text), pre/code (code blocks), and image-only elements.
var blockContainers = map[string]bool{
	"p": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true,
	"dd":         true, "dt": true,
	"td": true, "th": true,
	"figcaption": true, "div": true,
}

// transformBody mutates body in place: Readeck annotation wrappers are
// rewritten into styled spans, then every text run of every qualifying block
// container is wrapped in a koboSpan. It returns the span map in document
// order.
func transformBody(body *html.Node) []SpanRef {
	rewriteRDElements(body)
	idCounts := map[string]int{}
	countIDs(body, idCounts)
	spans := make([]SpanRef, 0)
	block := 0
	wrapContainers(body, &spans, &block, idCounts)
	return spans
}

// rewriteRDElements converts every element whose tag starts with "rd-"
// (Readeck's annotation wrappers such as rd-annotation) into a styled span:
// the tag becomes span and the original tag name becomes the class
// ("rd-annotation"), other attributes (id, data-annotation-*) are kept, and
// the children stay in place. Wrappers carry no text of their own, so the
// document text is unchanged; the class makes pre-existing Readeck highlights
// visible on the device through the .rd-annotation stylesheet rule. Empty
// wrappers (Readeck's note-marker siblings) become empty spans and simply
// carry no visible text.
func rewriteRDElements(n *html.Node) {
	if n.Type == html.ElementNode && isRDElement(n) {
		class := strings.ToLower(n.Data)
		n.Data = "span"
		replaced := false
		for i := range n.Attr {
			if strings.ToLower(n.Attr[i].Key) == "class" {
				n.Attr[i].Val = class
				replaced = true
				break
			}
		}
		if !replaced {
			n.Attr = append(n.Attr, html.Attribute{Key: "class", Val: class})
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rewriteRDElements(c)
	}
}

func isRDElement(n *html.Node) bool {
	return n.Type == html.ElementNode && strings.HasPrefix(strings.ToLower(n.Data), "rd-")
}

// countIDs records how often each id attribute occurs, so selectors can use
// the @id form only for ids that are unique in the document.
func countIDs(n *html.Node, counts map[string]int) {
	if n.Type == html.ElementNode {
		if id, ok := attrValue(n, "id"); ok && id != "" {
			counts[id]++
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		countIDs(c, counts)
	}
}

// wrapContainers walks the tree depth-first. A block container with text of
// its own gets its runs wrapped first (document order: its direct text
// precedes any nested containers), then nested containers are processed.
func wrapContainers(n *html.Node, spans *[]SpanRef, block *int, idCounts map[string]int) {
	if n.Type == html.ElementNode && isContainerElement(n) && hasWrappableText(n) {
		wrapRuns(n, spans, block, idCounts)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && !isKoboSpanElement(c) {
			wrapContainers(c, spans, block, idCounts)
		}
	}
}

// hasWrappableText reports whether n's subtree contains a non-whitespace text
// character outside of any nested block container (text inside nested
// containers is wrapped by those containers, not by n).
func hasWrappableText(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if textOutsideContainers(c) {
			return true
		}
	}
	return false
}

func textOutsideContainers(n *html.Node) bool {
	switch n.Type {
	case html.TextNode:
		return strings.IndexFunc(n.Data, func(r rune) bool { return !unicode.IsSpace(r) }) >= 0
	case html.ElementNode:
		if isContainerElement(n) {
			return false
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if textOutsideContainers(c) {
			return true
		}
	}
	return false
}

// wrapRuns wraps the direct-child text runs of one block container el.
//
// Run definition (the unit Kobo anchors highlights to): a maximal sequence of
// consecutive direct children that are either text nodes or inline elements —
// text nodes accumulate with adjacent text nodes, inline elements accumulate
// with adjacent inline elements; switching between the two kinds starts a new
// run. Nested block containers are never part of a run; comments and other
// non-content nodes end a run. Runs whose text is empty (e.g. an <img> or
// <br> between text runs) produce no span and no text — the surrounding runs
// stay exactly adjacent in the text.
//
// Spans are inserted as direct children of el in document order and numbered
// kobo.<block>.<run>; block is a 1-based counter over containers that receive
// at least one span, run restarts at 1 per container.
func wrapRuns(el *html.Node, spans *[]SpanRef, block *int, idCounts map[string]int) {
	elementText := textOf(el)

	type runGroup struct {
		nodes []*html.Node
		text  string
	}
	var groups []*runGroup
	var cur *runGroup
	curIsText := false
	flush := func() {
		if cur == nil {
			return
		}
		var b strings.Builder
		for _, m := range cur.nodes {
			appendText(&b, m)
		}
		cur.text = b.String()
		groups = append(groups, cur)
		cur = nil
	}
	for c := el.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			if !curIsText {
				flush()
				curIsText = true
			}
			if cur == nil {
				cur = &runGroup{}
			}
			cur.nodes = append(cur.nodes, c)
		case c.Type == html.ElementNode && !isContainerElement(c) && !containsBlockContainer(c):
			if curIsText {
				flush()
				curIsText = false
			}
			if cur == nil {
				cur = &runGroup{}
			}
			cur.nodes = append(cur.nodes, c)
		default:
			flush()
			curIsText = false
			cur = nil
		}
	}
	flush()

	anyText := false
	for _, g := range groups {
		if g.text != "" {
			anyText = true
			break
		}
	}
	if !anyText {
		return
	}

	*block++
	b := *block
	sel := selectorFor(el, idCounts)

	prefix := 0 // rune offset within elementText
	seg := 0
	gi := 0
	for c := el.FirstChild; c != nil; {
		if gi < len(groups) && groups[gi].nodes[0] == c {
			g := groups[gi]
			gi++
			if g.text == "" {
				c = c.NextSibling // empty run: no span, nodes stay in place
				continue
			}
			seg++
			span := &html.Node{
				Type: html.ElementNode,
				Data: "span",
				Attr: []html.Attribute{
					{Key: "class", Val: "koboSpan"},
					{Key: "id", Val: fmt.Sprintf("kobo.%d.%d", b, seg)},
				},
			}
			el.InsertBefore(span, c)
			for _, m := range g.nodes {
				el.RemoveChild(m)
				span.AppendChild(m)
			}
			*spans = append(*spans, SpanRef{
				SpanID:      fmt.Sprintf("kobo.%d.%d", b, seg),
				Selector:    sel,
				ElementText: elementText,
				PrefixRunes: prefix,
				SpanText:    g.text,
			})
			prefix += utf8.RuneCountInString(g.text)
			// After the move the span sits exactly where the group was: its
			// next sibling is the node that followed the group. Do NOT walk
			// through the captured pre-move sibling pointers — the moved
			// nodes now live under the span.
			c = span.NextSibling
		} else {
			prefix += utf8.RuneCountInString(textOf(c))
			c = c.NextSibling
		}
	}
}

// ---------------------------------------------------------------------------
// Selectors (Readeck dialect)
// ---------------------------------------------------------------------------

// selectorFor returns the body-relative XPath of el in Readeck's selector
// dialect: slash-separated steps starting at a direct child of <body>, never
// with a leading "/" or "//". Each step is either tag[i] (1-based position
// among same-tag element siblings) or tag[@id='…'] when the element carries a
// document-unique id. Readeck evaluates selectors with "./" prepended against
// its extracted article DOM; see docs/research/readeck-annotations-api.md.
func selectorFor(el *html.Node, idCounts map[string]int) string {
	var chain []*html.Node
	for n := el; n != nil && n.Type == html.ElementNode && !isBodyElement(n); n = n.Parent {
		chain = append(chain, n)
	}
	steps := make([]string, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		steps = append(steps, xpathStep(chain[i], idCounts))
	}
	return strings.Join(steps, "/")
}

func xpathStep(n *html.Node, idCounts map[string]int) string {
	tag := strings.ToLower(n.Data)
	if id, ok := stepID(n, idCounts); ok {
		return fmt.Sprintf("%s[@id=%s]", tag, id)
	}
	pos := 1
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.ElementNode && strings.ToLower(s.Data) == tag {
			pos++
		}
	}
	return fmt.Sprintf("%s[%d]", tag, pos)
}

// stepID returns the XPath literal (already quoted) for a usable id
// attribute, or ok=false so the caller falls back to the positional form.
// The @id form is only used for ids that are non-empty, unique in the
// document, free of whitespace, and quotable without XPath concat() hacks.
func stepID(n *html.Node, idCounts map[string]int) (string, bool) {
	id, ok := attrValue(n, "id")
	if !ok || id == "" || idCounts[id] != 1 {
		return "", false
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return "", false
	}
	switch {
	case !strings.Contains(id, "'"):
		return "'" + id + "'", true
	case !strings.Contains(id, `"`):
		return `"` + id + `"`, true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// HTML node helpers
// ---------------------------------------------------------------------------

func isBodyElement(n *html.Node) bool {
	return n.Type == html.ElementNode && strings.ToLower(n.Data) == "body"
}

func isContainerElement(n *html.Node) bool {
	return n.Type == html.ElementNode && blockContainers[strings.ToLower(n.Data)]
}

func isKoboSpanElement(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == "class" && hasWord(a.Val, "koboSpan") {
			return true
		}
	}
	return false
}

// containsBlockContainer reports whether n's subtree contains any element
// from the block container set (such content can never be part of an inline
// run of an ancestor).
func containsBlockContainer(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if isContainerElement(n) {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if containsBlockContainer(c) {
			return true
		}
	}
	return false
}

func attrValue(n *html.Node, key string) (string, bool) {
	if n.Type != html.ElementNode {
		return "", false
	}
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == key {
			return a.Val, true
		}
	}
	return "", false
}

func hasWord(s, word string) bool {
	for _, f := range strings.Fields(s) {
		if f == word {
			return true
		}
	}
	return false
}

// textOf returns the concatenation of all descendant text nodes of n.
func textOf(n *html.Node) string {
	var b strings.Builder
	appendText(&b, n)
	return b.String()
}

func appendText(b *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		appendText(b, c)
	}
}

func findBody(doc *html.Node) *html.Node {
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || !isBodyElement(c) {
			continue
		}
		return c
	}
	// html.Parse always synthesizes <html><body>, but search defensively:
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || strings.ToLower(c.Data) != "html" {
			continue
		}
		for cc := c.FirstChild; cc != nil; cc = cc.NextSibling {
			if cc.Type == html.ElementNode && isBodyElement(cc) {
				return cc
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// XHTML chapter assembly and XML serialization
// ---------------------------------------------------------------------------

const (
	xhtmlNS = "http://www.w3.org/1999/xhtml"
	epubNS  = "http://www.idpf.org/2007/ops"
)

const xmlDecl = `<?xml version="1.0" encoding="utf-8"?>` + "\n"

// buildChapterTree assembles the XHTML document for OEBPS/xhtml/ch001.xhtml:
// a single chapter carrying the (transformed) article body, preceded by the
// empty div.kobostylehacks marker div Kobo expects (kepubify convention).
func buildChapterTree(meta Meta, srcBody *html.Node) *html.Node {
	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = "Untitled"
	}
	htmlEl := el("html", attr("xmlns", xhtmlNS))

	head := el("head")
	head.AppendChild(el("meta", attr("charset", "utf-8")))
	titleEl := el("title")
	titleEl.AppendChild(&html.Node{Type: html.TextNode, Data: title})
	head.AppendChild(titleEl)
	head.AppendChild(el("link",
		attr("rel", "stylesheet"),
		attr("type", "text/css"),
		attr("href", "../style.css"),
	))
	htmlEl.AppendChild(head)

	body := el("body")
	body.AppendChild(el("div", attr("class", "kobostylehacks")))
	for srcBody.FirstChild != nil {
		c := srcBody.FirstChild
		srcBody.RemoveChild(c) // x/net/html requires detached children
		body.AppendChild(c)
	}
	htmlEl.AppendChild(body)
	return htmlEl
}

// navDocument builds the EPUB3 navigation document (OEBPS/xhtml/nav.xhtml).
// EPUB3 requires a nav document in the manifest; Kobo ignores it.
func navDocument(meta Meta) []byte {
	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = "Untitled"
	}
	htmlEl := el("html",
		attr("xmlns", xhtmlNS),
		attr("xmlns:epub", epubNS),
	)
	head := el("head")
	titleEl := el("title")
	titleEl.AppendChild(&html.Node{Type: html.TextNode, Data: title})
	head.AppendChild(titleEl)
	htmlEl.AppendChild(head)

	nav := el("nav", attr("epub:type", "toc"), attr("id", "toc"))
	ol := el("ol")
	li := el("li")
	a := el("a", attr("href", "ch001.xhtml"))
	a.AppendChild(&html.Node{Type: html.TextNode, Data: title})
	li.AppendChild(a)
	ol.AppendChild(li)
	nav.AppendChild(ol)
	body := el("body")
	body.AppendChild(nav)
	htmlEl.AppendChild(body)
	return renderXHTML(htmlEl)
}

func el(data string, attrs ...html.Attribute) *html.Node {
	return &html.Node{Type: html.ElementNode, Data: data, Attr: attrs}
}

func attr(key, val string) html.Attribute {
	return html.Attribute{Key: key, Val: val}
}

// renderXHTML serializes a node tree as XML-well-formed XHTML. html.Render's
// HTML serialization is not usable here: it emits unclosed void elements and
// HTML entities such as &nbsp; that are not predefined in XML.
func renderXHTML(root *html.Node) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl)
	writeXMLNode(&b, root)
	return b.Bytes()
}

// voidElements are serialized self-closed so the output parses both as XML
// (encoding/xml, EPUB reading systems) and as HTML (x/net/html).
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

func writeXMLNode(w *bytes.Buffer, n *html.Node) {
	switch n.Type {
	case html.ElementNode:
		tag := n.Data
		w.WriteByte('<')
		w.WriteString(tag)
		for _, a := range n.Attr {
			w.WriteByte(' ')
			w.WriteString(a.Key)
			w.WriteString(`="`)
			w.WriteString(escapeXMLAttr(a.Val))
			w.WriteByte('"')
		}
		if voidElements[tag] && n.FirstChild == nil {
			w.WriteString("/>")
			return
		}
		w.WriteByte('>')
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			writeXMLNode(w, c)
		}
		w.WriteString("</")
		w.WriteString(tag)
		w.WriteByte('>')
	case html.TextNode:
		w.WriteString(escapeXMLText(n.Data))
	case html.CommentNode:
		w.WriteString("<!--")
		w.WriteString(strings.ReplaceAll(n.Data, "--", "- -"))
		w.WriteString("-->")
	}
}

// escapeXMLText escapes the three characters that are special in XML text.
// Everything else — including non-breaking spaces and any Unicode — is
// written raw as UTF-8 (valid XML; no &nbsp;-style entity problems).
func escapeXMLText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func escapeXMLAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// EPUB assembly
// ---------------------------------------------------------------------------

const styleCSS = `/* readeckobo kepub reader styles.
   Kobo's reader largely ignores CSS; these rules serve other EPUB readers. */

body {
	margin: 1em 1.2em;
	padding: 0;
	line-height: 1.5;
}

p, li, blockquote, dd, dt, td, th, figcaption {
	margin: 0.5em 0;
}

h1, h2, h3, h4, h5, h6 {
	line-height: 1.25;
	margin: 0.8em 0 0.4em;
}

img, video, svg {
	max-width: 100%;
	height: auto;
}

/* kepub convention marker div, hidden */
div.kobostylehacks {
	display: none;
}

/* Pre-existing Readeck highlights: Build rewrites rd-annotation wrappers
   into <span class="rd-annotation"> and this rule makes them visible.
   Kobo's kepub renderer supports background-color on spans. */
.rd-annotation {
	background-color: rgba(255, 235, 59, 0.45);
}
`

const containerXML = xmlDecl +
	`<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">` + "\n" +
	`  <rootfiles>` + "\n" +
	`    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>` + "\n" +
	`  </rootfiles>` + "\n" +
	`</container>` + "\n"

// assembleEPUB zips the kepub: mimetype (stored, first), container.xml, the
// OPF package document, the stylesheet, the EPUB3 nav document and the single
// chapter.
func assembleEPUB(chapterXML []byte, meta Meta) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, content []byte, store bool) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if store {
			h.Method = zip.Store
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	}
	entries := []struct {
		name    string
		content []byte
		store   bool
	}{
		{"mimetype", []byte("application/epub+zip"), true},
		{"META-INF/container.xml", []byte(containerXML), false},
		{"OEBPS/content.opf", []byte(buildOPF(meta)), false},
		{"OEBPS/style.css", []byte(styleCSS), false},
		{"OEBPS/xhtml/nav.xhtml", navDocument(meta), false},
		{"OEBPS/xhtml/ch001.xhtml", chapterXML, false},
	}
	for _, e := range entries {
		if err := add(e.name, e.content, e.store); err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildOPF renders OEBPS/content.opf (EPUB3 package document). dc:title /
// dc:creator come from meta; dc:language is hardcoded to "en" (Meta carries
// no language — open question in docs/research/kepub-generation.md). The
// identifier is a stable hash derived from source URL + title.
func buildOPF(meta Meta) string {
	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = "Untitled"
	}
	var b strings.Builder
	b.WriteString(xmlDecl)
	b.WriteString(`<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">` + "\n")
	b.WriteString(`  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">` + "\n")
	fmt.Fprintf(&b, "    <dc:identifier id=\"pub-id\">%s</dc:identifier>\n", opfIdentifier(meta))
	fmt.Fprintf(&b, "    <dc:title>%s</dc:title>\n", escapeXMLText(title))
	if author := strings.TrimSpace(meta.Author); author != "" {
		fmt.Fprintf(&b, "    <dc:creator>%s</dc:creator>\n", escapeXMLText(author))
	}
	b.WriteString("    <dc:language>en</dc:language>\n")
	fmt.Fprintf(&b, "    <meta property=\"dcterms:modified\">%s</meta>\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("  </metadata>\n")
	b.WriteString(`  <manifest>` + "\n")
	b.WriteString(`    <item id="nav" href="xhtml/nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>` + "\n")
	b.WriteString(`    <item id="ch1" href="xhtml/ch001.xhtml" media-type="application/xhtml+xml"/>` + "\n")
	b.WriteString(`    <item id="css" href="style.css" media-type="text/css"/>` + "\n")
	b.WriteString(`  </manifest>` + "\n")
	b.WriteString(`  <spine>` + "\n")
	b.WriteString(`    <itemref idref="ch1"/>` + "\n")
	b.WriteString(`  </spine>` + "\n")
	b.WriteString(`</package>` + "\n")
	return b.String()
}

// opfIdentifier derives a stable, source-based unique identifier for the
// package document.
func opfIdentifier(meta Meta) string {
	sum := sha256.Sum256([]byte("readeckobo\x00" + meta.SourceURL + "\x00" + meta.Title))
	return "urn:readeckobo:" + hex.EncodeToString(sum[:16])
}
