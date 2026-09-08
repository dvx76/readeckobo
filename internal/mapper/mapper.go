// Package mapper converts device-side Kobo highlight ranges (span ids +
// offsets) into Readeck annotation ranges (body-relative XPath selectors +
// rune offsets), using the span map produced by internal/kepub.
//
// Two levels, in order:
//
//   - L1 (exact): both the start and end span ids from the device's
//     StartContainerPath/EndContainerPath resolve in the span map and belong
//     to the same block element. The absolute rune range inside the block's
//     ElementText is then exactly
//     [PrefixRunes(start)+StartOffset, PrefixRunes(end)+EndOffset), and the
//     selector is the block's recorded selector.
//   - L2 (fallback): used when a span id is unknown or the range crosses
//     blocks. The device highlight text is located in the current (served)
//     article HTML with whitespace-normalized comparison — first occurrence
//     wins, preferring the start block when its span resolved — and the
//     found block's positional/id selector is computed in Readeck's dialect.
//
// If both levels fail the result carries a Reason and the caller reports the
// item as "skipped". See docs/research/mapping-and-endpoints.md.
package mapper

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"readeckobo/internal/kepub"
)

// DeviceRange is one Kobo Bookmark row, expressed against the generated kepub:
// start/end paths carry the span id in a fragment
// ("OEBPS/xhtml/ch001.xhtml#kobo.2.1" or "span#kobo\.2\.1"), offsets are rune
// offsets within the respective span's text, and Text is the highlight text as
// stored on the device.
type DeviceRange struct {
	StartPath   string
	EndPath     string
	StartOffset int
	EndOffset   int
	Text        string
}

// MappedRange is a Readeck annotation range: body-relative XPath selectors
// and rune offsets (end exclusive) into the selected element's concatenated
// descendant text.
type MappedRange struct {
	StartSelector string
	StartOffset   int
	EndSelector   string
	EndOffset     int
	Text          string // the device highlight text, passed through
}

// Result is the outcome of mapping one device range.
type Result struct {
	Range  *MappedRange
	Level  string // "l1" or "l2"; empty when skipped
	Reason string // non-empty when Range == nil (skip reason)
}

// spanIDRe matches a trailing "kobo.<block>.<run>" span id fragment. The path
// may use "#" or the URL-escaped "%23"; CSS-escaped dots ("kobo\.2\.1") are
// unescaped first.
var spanIDRe = regexp.MustCompile(`(?:#|%23|^)(kobo\.\d+\.\d+)$`)

// ParseSpanID extracts the "kobo.<block>.<run>" span id from a container path
// (e.g. "OEBPS/xhtml/ch001.xhtml#kobo.2.1" → "kobo.2.1"). It reports ok=false
// when the path carries no recognizable span id.
func ParseSpanID(path string) (string, bool) {
	p := strings.ReplaceAll(path, `\.`, ".")
	m := spanIDRe.FindStringSubmatch(strings.TrimSpace(p))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Map converts a device range to a Readeck range, trying L1 then L2.
func Map(spanMap []kepub.SpanRef, articleHTML string, dr DeviceRange) Result {
	byID := make(map[string]kepub.SpanRef, len(spanMap))
	for _, s := range spanMap {
		byID[s.SpanID] = s
	}

	if res, ok := mapL1(byID, dr); ok {
		return res
	}

	// L1 failed; remember which spans (if any) were found so L2 can prefer
	// the start block.
	startID, _ := ParseSpanID(dr.StartPath)
	startRef, startKnown := byID[startID]
	return mapL2(articleHTML, dr, startRef, startKnown)
}

// mapL1 computes the exact range when both spans resolve to the same block.
func mapL1(byID map[string]kepub.SpanRef, dr DeviceRange) (Result, bool) {
	startID, ok := ParseSpanID(dr.StartPath)
	if !ok {
		return Result{Reason: "start path has no span id: " + dr.StartPath}, false
	}
	endID, ok := ParseSpanID(dr.EndPath)
	if !ok {
		return Result{Reason: "end path has no span id: " + dr.EndPath}, false
	}
	startRef, ok := byID[startID]
	if !ok {
		return Result{Reason: "unknown span " + startID}, false
	}
	endRef, ok := byID[endID]
	if !ok {
		return Result{Reason: "unknown span " + endID}, false
	}
	if startRef.Selector != endRef.Selector {
		return Result{Reason: "cross-block range (" + startID + " -> " + endID + ")"}, false
	}

	elementLen := utf8.RuneCountInString(startRef.ElementText)
	start := clamp(startRef.PrefixRunes+dr.StartOffset, 0, elementLen)
	end := clamp(endRef.PrefixRunes+dr.EndOffset, 0, elementLen) // clamp end to element length
	if end <= start {
		return Result{Reason: "empty range after clamping"}, false
	}

	return Result{
		Level: "l1",
		Range: &MappedRange{
			StartSelector: startRef.Selector,
			StartOffset:   start,
			EndSelector:   endRef.Selector,
			EndOffset:     end,
			Text:          dr.Text,
		},
	}, true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mapL2 locates the device text in the current article HTML. When startKnown
// is true (the start span resolved but the range crossed blocks or the end was
// unknown), the start block is searched first; otherwise every block in
// document order is searched and the first occurrence wins.
func mapL2(articleHTML string, dr DeviceRange, startRef kepub.SpanRef, startKnown bool) Result {
	if strings.TrimSpace(dr.Text) == "" {
		return Result{Reason: "no device text to locate"}
	}
	doc, err := html.Parse(strings.NewReader(articleHTML))
	if err != nil {
		return Result{Reason: "cannot parse article HTML: " + err.Error()}
	}
	body := findBody(doc)
	if body == nil {
		return Result{Reason: "article HTML has no body"}
	}
	idCounts := map[string]int{}
	countIDs(body, idCounts)

	needle := dr.Text

	// Prefer the start block when we know which block it is.
	if startKnown {
		if node := resolveSelector(body, startRef.Selector); node != nil {
			if start, end, ok := matchNormalized(textOf(node), needle); ok {
				return l2Result(node, idCounts, start, end, dr.Text)
			}
		}
	}

	// Document-wide first occurrence, in document order.
	for _, blk := range blocksIn(body) {
		if start, end, ok := matchNormalized(textOf(blk), needle); ok {
			return l2Result(blk, idCounts, start, end, dr.Text)
		}
	}

	return Result{Reason: "device text not found in article"}
}

func l2Result(node *html.Node, idCounts map[string]int, start, end int, text string) Result {
	sel := selectorFor(node, idCounts)
	return Result{
		Level: "l2",
		Range: &MappedRange{
			StartSelector: sel,
			StartOffset:   start,
			EndSelector:   sel,
			EndOffset:     end,
			Text:          text,
		},
	}
}

// ---------------------------------------------------------------------------
// HTML helpers (block traversal, text, selectors) — mirror the conventions of
// internal/kepub (body-relative, tag[i] or tag[@id='…'], no leading "/").
// ---------------------------------------------------------------------------

// blockTags mirrors kepub's container set: the paragraph-like elements whose
// text Kobo highlights anchor to.
var blockTags = map[string]bool{
	"p": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true,
	"dd":         true, "dt": true,
	"td": true, "th": true,
	"figcaption": true, "div": true,
}

func findBody(doc *html.Node) *html.Node {
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.ToLower(c.Data) == "body" {
			return c
		}
	}
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || strings.ToLower(c.Data) != "html" {
			continue
		}
		for cc := c.FirstChild; cc != nil; cc = cc.NextSibling {
			if cc.Type == html.ElementNode && strings.ToLower(cc.Data) == "body" {
				return cc
			}
		}
	}
	return nil
}

// blocksIn returns block elements in document order.
func blocksIn(body *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && blockTags[strings.ToLower(n.Data)] {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	return out
}

// countIDs counts id attributes so selectors only use the @id form for
// document-unique ids.
func countIDs(n *html.Node, counts map[string]int) {
	if n.Type == html.ElementNode {
		if id := attrValue(n, "id"); id != "" {
			counts[id]++
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		countIDs(c, counts)
	}
}

// selectorFor builds the body-relative XPath for n in Readeck's dialect.
func selectorFor(n *html.Node, idCounts map[string]int) string {
	var chain []*html.Node
	for m := n; m != nil && m.Type == html.ElementNode && !isBody(m); m = m.Parent {
		chain = append(chain, m)
	}
	steps := make([]string, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		steps = append(steps, xpathStep(chain[i], idCounts))
	}
	return strings.Join(steps, "/")
}

func xpathStep(n *html.Node, idCounts map[string]int) string {
	tag := strings.ToLower(n.Data)
	if id := stepID(n, idCounts); id != "" {
		return tag + "[@id=" + id + "]"
	}
	pos := 1
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.ElementNode && strings.ToLower(s.Data) == tag {
			pos++
		}
	}
	return tag + "[" + strconv.Itoa(pos) + "]"
}

// stepID returns a quoted XPath literal for a usable id, or "" to fall back to
// the positional form (mirrors kepub.stepID).
func stepID(n *html.Node, idCounts map[string]int) string {
	id := attrValue(n, "id")
	if id == "" || idCounts[id] != 1 {
		return ""
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return ""
	}
	switch {
	case !strings.Contains(id, "'"):
		return "'" + id + "'"
	case !strings.Contains(id, `"`):
		return `"` + id + `"`
	default:
		return ""
	}
}

// resolveSelector resolves a body-relative Readeck-dialect selector to a node,
// or returns nil when it does not resolve to exactly one node. Only the two
// step forms the package produces (tag[i] / tag[@id='…']) are supported.
func resolveSelector(body *html.Node, sel string) *html.Node {
	if sel == "" || strings.HasPrefix(sel, "/") || strings.Contains(sel, "//") {
		return nil
	}
	cands := []*html.Node{body}
	for _, step := range strings.Split(sel, "/") {
		next := resolveStep(cands, step)
		if len(next) == 0 {
			return nil
		}
		cands = next
	}
	if len(cands) != 1 {
		return nil
	}
	return cands[0]
}

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
				if e.Type == html.ElementNode && strings.EqualFold(e.Data, tag) && attrValue(e, "id") == want {
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

// ---------------------------------------------------------------------------
// Text matching (whitespace-normalized)
// ---------------------------------------------------------------------------

// matchNormalized finds the first occurrence of needle in rawText comparing
// whitespace runs as a single space (Readeck's extraction may normalize
// whitespace; device text may contain newlines). It returns rune start
// (inclusive) and rune end (exclusive) indices into rawText.
func matchNormalized(rawText, needle string) (start, end int, ok bool) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return 0, 0, false
	}
	raw := []rune(rawText)
	nNorm, _ := collapseRunes([]rune(needle))
	rNorm, rIdx := collapseRunes(raw)
	if len(nNorm) == 0 || len(nNorm) > len(rNorm) {
		return 0, 0, false
	}
	pos := indexRunes(rNorm, nNorm)
	if pos < 0 {
		return 0, 0, false
	}
	start = rIdx[pos]
	last := pos + len(nNorm) - 1
	end = rIdx[last] + 1
	if end > len(raw) {
		end = len(raw)
	}
	if end < start {
		end = start
	}
	return start, end, true
}

// collapseRunes collapses maximal whitespace runs to a single space and
// records, for each collapsed rune, the raw rune index it came from.
func collapseRunes(in []rune) ([]rune, []int) {
	out := make([]rune, 0, len(in))
	idx := make([]int, 0, len(in))
	prevSpace := false
	for i, r := range in {
		if unicode.IsSpace(r) {
			if prevSpace {
				continue
			}
			prevSpace = true
			out = append(out, ' ')
			idx = append(idx, i)
		} else {
			prevSpace = false
			out = append(out, r)
			idx = append(idx, i)
		}
	}
	return out, idx
}

// indexRunes returns the index of the first occurrence of needle in hay, or -1.
func indexRunes(hay, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Node helpers
// ---------------------------------------------------------------------------

func isBody(n *html.Node) bool {
	return n.Type == html.ElementNode && strings.ToLower(n.Data) == "body"
}

func attrValue(n *html.Node, key string) string {
	if n.Type != html.ElementNode {
		return ""
	}
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) == key {
			return a.Val
		}
	}
	return ""
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
