package mapper

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"readeckobo/internal/kepub"
)

var testMeta = kepub.Meta{
	Title:     "Test Article",
	Author:    "Ada Example",
	SourceURL: "https://example.com/article",
}

// fixtureHTML reads the real Readeck served-article fixture.
func fixtureHTML(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../kepub/testdata/readeck-article.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

// fixtureArtifact builds the kepub from the real Readeck fixture, mirroring the
// service path (the fixture is the served article fragment).
func fixtureArtifact(t *testing.T) *kepub.Artifact {
	t.Helper()
	a, err := kepub.Build(fixtureHTML(t), testMeta)
	if err != nil {
		t.Fatalf("kepub.Build: %v", err)
	}
	return a
}

func refByID(t *testing.T, a *kepub.Artifact, id string) kepub.SpanRef {
	t.Helper()
	for _, s := range a.Spans {
		if s.SpanID == id {
			return s
		}
	}
	t.Fatalf("span %s not in map", id)
	return kepub.SpanRef{}
}

func runeSlice(s string, start, end int) string {
	r := []rune(s)
	if start < 0 || end > len(r) || start > end {
		return ""
	}
	return string(r[start:end])
}

func TestParseSpanID(t *testing.T) {
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"OEBPS/xhtml/ch001.xhtml#kobo.2.1", "kobo.2.1", true},
		{"OEBPS/xhtml/ch001.xhtml#kobo.2.1", "kobo.2.1", true},
		{"span#kobo\\.5\\.2", "kobo.5.2", true},
		{"span#kobo\\.5\\.2", "kobo.5.2", true},
		{"OEBPS/xhtml/ch001.xhtml%23kobo.3.1", "kobo.3.1", true},
		{"OEBPS/xhtml/ch001.xhtml", "", false},
		{"", "", false},
		{"kobo.9.4", "kobo.9.4", true},
	}
	for _, tc := range cases {
		got, ok := ParseSpanID(tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseSpanID(%q) = %q,%v want %q,%v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMapL1SingleSpan(t *testing.T) {
	a := fixtureArtifact(t)
	res := Map(a.Spans, "", DeviceRange{
		StartPath:   "OEBPS/xhtml/ch001.xhtml#kobo.1.1",
		EndPath:     "OEBPS/xhtml/ch001.xhtml#kobo.1.1",
		StartOffset: 0,
		EndOffset:   5,
		Text:        "ALPHA",
	})
	if res.Level != "l1" || res.Range == nil {
		t.Fatalf("expected l1 result, got %+v", res)
	}
	if res.Range.StartSelector != `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']` ||
		res.Range.EndSelector != res.Range.StartSelector {
		t.Errorf("unexpected selector: %+v", res.Range)
	}
	if res.Range.StartOffset != 0 || res.Range.EndOffset != 5 {
		t.Errorf("unexpected offsets: %+v", res.Range)
	}
	ref := refByID(t, a, "kobo.1.1")
	if got := runeSlice(ref.ElementText, res.Range.StartOffset, res.Range.EndOffset); got != "ALPHA" {
		t.Errorf("sliced text = %q, want %q", got, "ALPHA")
	}
}

func TestMapL1MidSpanNestedInline(t *testing.T) {
	a := fixtureArtifact(t)
	// kobo.2.2 is <strong>bold inside</strong>, prefix 6 in the bravo block.
	res := Map(a.Spans, "", DeviceRange{
		StartPath:   "span#kobo\\.2\\.2",
		EndPath:     "span#kobo\\.2\\.2",
		StartOffset: 0,
		EndOffset:   4,
		Text:        "bold",
	})
	if res.Level != "l1" || res.Range == nil {
		t.Fatalf("expected l1 result, got %+v", res)
	}
	ref := refByID(t, a, "kobo.2.2")
	if res.Range.StartOffset != 6 || res.Range.EndOffset != 10 {
		t.Errorf("unexpected offsets: %+v (prefix %d)", res.Range, ref.PrefixRunes)
	}
	if got := runeSlice(ref.ElementText, res.Range.StartOffset, res.Range.EndOffset); got != "bold" {
		t.Errorf("sliced text = %q, want %q", got, "bold")
	}
	if res.Range.StartSelector != `section[1]/article[1]/p[@id='uQ.GbYH.p-bravo']` {
		t.Errorf("unexpected selector: %q", res.Range.StartSelector)
	}
}

func TestMapL1MultiSpanSameBlock(t *testing.T) {
	a := fixtureArtifact(t)
	// kobo.2.1 (prefix 0) through kobo.2.5 (prefix 35) + 5 runes.
	res := Map(a.Spans, "", DeviceRange{
		StartPath:   "#kobo.2.1",
		EndPath:     "#kobo.2.5",
		StartOffset: 0,
		EndOffset:   5,
		Text:        "BRAVO bold inside and italic inside plus",
	})
	if res.Level != "l1" || res.Range == nil {
		t.Fatalf("expected l1 result, got %+v", res)
	}
	if res.Range.StartOffset != 0 || res.Range.EndOffset != 40 {
		t.Errorf("unexpected offsets: %+v", res.Range)
	}
	ref := refByID(t, a, "kobo.2.1")
	if got := runeSlice(ref.ElementText, res.Range.StartOffset, res.Range.EndOffset); got != "BRAVO bold inside and italic inside plus" {
		t.Errorf("sliced text = %q", got)
	}
}

func TestMapL1ClampsEndToElementLength(t *testing.T) {
	a := fixtureArtifact(t)
	res := Map(a.Spans, "", DeviceRange{
		StartPath:   "#kobo.1.1",
		EndPath:     "#kobo.1.1",
		StartOffset: 0,
		EndOffset:   99999,
		Text:        "ALPHA one two three four five six seven eight nine ten.",
	})
	if res.Level != "l1" || res.Range == nil {
		t.Fatalf("expected l1 result, got %+v", res)
	}
	ref := refByID(t, a, "kobo.1.1")
	want := utf8.RuneCountInString(ref.ElementText)
	if res.Range.EndOffset != want {
		t.Errorf("end offset = %d, want %d (clamped to element length)", res.Range.EndOffset, want)
	}
}

func TestMapCrossBlockFallsBackToL2StartBlock(t *testing.T) {
	a := fixtureArtifact(t)
	// Start/end spans are in different blocks (kobo.1.1 vs kobo.2.1): L1
	// fails, L2 locates the text and, because the start span resolved,
	// searches the start block first.
	alpha := refByID(t, a, "kobo.1.1")
	needle := "one two three four five six seven eight nine ten."
	res := Map(a.Spans, fixtureHTML(t), DeviceRange{
		StartPath:   "#kobo.1.1",
		EndPath:     "#kobo.2.1",
		StartOffset: 0,
		EndOffset:   0,
		Text:        needle,
	})
	if res.Level != "l2" || res.Range == nil {
		t.Fatalf("expected l2 result, got %+v", res)
	}
	wantStart := utf8.RuneCountInString("ALPHA ")
	wantEnd := wantStart + utf8.RuneCountInString(needle)
	if res.Range.StartOffset != wantStart || res.Range.EndOffset != wantEnd {
		t.Errorf("offsets = %d..%d, want %d..%d", res.Range.StartOffset, res.Range.EndOffset, wantStart, wantEnd)
	}
	if res.Range.StartSelector != `section[1]/article[1]/p[@id='uQ.GbYH.p-alpha']` {
		t.Errorf("selector = %q", res.Range.StartSelector)
	}
	if got := runeSlice(alpha.ElementText, res.Range.StartOffset, res.Range.EndOffset); got != needle {
		t.Errorf("sliced text = %q, want %q", got, needle)
	}
}

func TestMapUnknownSpanFallsBackToL2(t *testing.T) {
	a := fixtureArtifact(t)
	needle := "CHARLIE delta echo foxtrot golf hotel india juliet kilo lima."
	res := Map(a.Spans, fixtureHTML(t), DeviceRange{
		StartPath:   "#kobo.99.1",
		EndPath:     "#kobo.99.1",
		StartOffset: 0,
		EndOffset:   0,
		Text:        needle,
	})
	if res.Level != "l2" || res.Range == nil {
		t.Fatalf("expected l2 result, got %+v", res)
	}
	if res.Range.StartSelector != "section[1]/article[1]/p[3]" {
		t.Errorf("selector = %q, want section[1]/article[1]/p[3]", res.Range.StartSelector)
	}
	if res.Range.StartOffset != 0 || res.Range.EndOffset != utf8.RuneCountInString(needle) {
		t.Errorf("offsets = %d..%d", res.Range.StartOffset, res.Range.EndOffset)
	}
}

func TestMapWhitespaceNormalized(t *testing.T) {
	a := fixtureArtifact(t)
	// Device text with newlines / double spaces still matches the article.
	needle := "one  two\n three four five six seven eight nine ten."
	res := Map(a.Spans, fixtureHTML(t), DeviceRange{
		StartPath: "#kobo.99.1",
		EndPath:   "#kobo.99.1",
		Text:      needle,
	})
	if res.Level != "l2" || res.Range == nil {
		t.Fatalf("expected l2 result, got %+v", res)
	}
	alpha := refByID(t, a, "kobo.1.1")
	if got := runeSlice(alpha.ElementText, res.Range.StartOffset, res.Range.EndOffset); !strings.Contains(got, "one two three four five six seven eight nine ten") {
		t.Errorf("matched range does not contain the normalized text: %q", got)
	}
}

func TestMapTextNotFound(t *testing.T) {
	a := fixtureArtifact(t)
	res := Map(a.Spans, fixtureHTML(t), DeviceRange{
		StartPath:   "#kobo.99.1",
		EndPath:     "#kobo.99.1",
		StartOffset: 0,
		EndOffset:   0,
		Text:        "this text does not exist anywhere in the article at all",
	})
	if res.Range != nil {
		t.Fatalf("expected nil range, got %+v", res.Range)
	}
	if !strings.Contains(res.Reason, "not found") {
		t.Errorf("reason = %q, want a 'not found' reason", res.Reason)
	}
}

func TestMapCrossBlockTextSpansBlocksSkipped(t *testing.T) {
	a := fixtureArtifact(t)
	// Text that runs across two blocks cannot be contained by any single
	// block element, so both L1 (cross-block) and L2 (no containing block)
	// fail → skipped.
	res := Map(a.Spans, fixtureHTML(t), DeviceRange{
		StartPath:   "#kobo.1.1",
		EndPath:     "#kobo.2.1",
		StartOffset: 0,
		EndOffset:   0,
		Text:        "ALPHA one two three four five six seven eight nine ten.BRAVO bold",
	})
	if res.Range != nil {
		t.Fatalf("expected nil range, got %+v", res.Range)
	}
	if res.Reason == "" {
		t.Error("expected a skip reason")
	}
}
