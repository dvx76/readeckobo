package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Sentinel errors for timestamp parsing (callers decide whether an empty
// timestamp is acceptable in context).
var (
	errEmptyTime   = errors.New("empty timestamp")
	errInvalidTime = errors.New("invalid timestamp")
)

// Kobo stores timestamps as UTC ISO8601 with a trailing Z
// (%Y-%m-%dT%H:%M:%SZ, calibre TIMESTAMP_STRING). We output that form and
// parse leniently (offsets, fractional seconds, naive UTC).
const koboTimeLayout = "2006-01-02T15:04:05Z"

var koboTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05", // naive: assume UTC
	"2006-01-02 15:04:05",
}

// parseKoboTime parses a device/server timestamp leniently. Naive timestamps
// are assumed to be UTC (as on the device). A parsed time is normalized to
// UTC without sub-second precision by formatKoboTime.
func parseKoboTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errEmptyTime
	}
	for _, layout := range koboTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errInvalidTime
}

func formatKoboTime(t time.Time) string {
	return t.UTC().Format(koboTimeLayout)
}

// newUUIDv4 returns a random RFC 4122 version-4 UUID formatted as the
// conventional 8-4-4-4-12 hex string (e.g. "8f14ab90-...."). The modern shelf
// shape (DbVersion >= 64) carries a real Id column — calibre writes a UUID
// there — so the agent generates one from crypto/rand (unpredictable,
// collision-free in practice).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// maxTitleRunes bounds the sanitized title prefix so file names stay well
// under common filesystem limits (255 bytes on vfat).
const maxTitleRunes = 90

// forbiddenFileNameRunes are the characters that break paths on Linux and/or
// vfat (the onboard filesystem type is unverified — see docs/AGENT.md).
func forbiddenFileNameRune(r rune) bool {
	switch r {
	case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
		return true
	}
	return unicode.IsControl(r)
}

// sanitizeTitle produces a filesystem-safe title fragment: forbidden
// characters become spaces, whitespace collapses, leading dots (and the
// separators right after them) are dropped so dotfiles and ".." cannot occur,
// trailing dots/spaces are stripped (vfat), and the result is capped.
func sanitizeTitle(title string) string {
	var b strings.Builder
	spacePending := false
	for _, r := range title {
		if forbiddenFileNameRune(r) || unicode.IsSpace(r) {
			spacePending = b.Len() > 0
			continue
		}
		if spacePending && b.Len() > 0 {
			b.WriteRune(' ')
		}
		spacePending = false
		b.WriteRune(r)
		if b.Len() >= maxTitleRunes {
			break
		}
	}
	out := b.String()
	// Drop leading dots and any separator that follows them (".", "..",
	// "...", ".. Name" → "Name").
	for len(out) > 0 {
		if out[0] == '.' {
			out = out[1:]
			out = strings.TrimLeft(out, " ")
			continue
		}
		break
	}
	out = strings.TrimRight(out, " .")
	if out == "" {
		return "Untitled"
	}
	return out
}

// sanitizeID keeps only URL/path-safe characters from a server bookmark id.
// Readeck ids are short alphanumerics, so this is purely defensive.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// kepubFilename is the on-device name for an article's kepub:
// "<sanitized-title>-<bookmark_id>.kepub.epub". An empty bookmark id (only
// the synthetic fixture uses this) yields "<sanitized-title>.kepub.epub".
func kepubFilename(title, bookmarkID string) string {
	base := sanitizeTitle(title)
	if id := sanitizeID(bookmarkID); id != "" {
		base += "-" + id
	}
	return base + ".kepub.epub"
}

// kepubRelDir is the kepub store directory relative to the onboard root.
const kepubRelDir = ".kobo/readeck"

// kepubRelPath returns the kepub path relative to the onboard root.
func kepubRelPath(filename string) string {
	return filepath.ToSlash(filepath.Join(kepubRelDir, filename))
}

// volumeIDFor is the ContentID/VolumeID the device database uses for a book
// row: file://<onboard>/<relpath> (see docs/research/kobo-db-schema.md §4).
func volumeIDFor(onboard, filename string) string {
	return "file://" + filepath.ToSlash(filepath.Join(onboard, kepubRelDir, filename))
}

// dirVolumeSuffix is the directory fragment used to find *our* book rows in
// the device DB without depending on the exact onboard mount path:
// ".kobo/readeck/" (as a LIKE tail, e.g. '%/.kobo/readeck/'). The managed
// directory is unique to this agent, so any row under it is ours. The
// constant contains no LIKE wildcard characters.
const dirVolumeSuffix = "/.kobo/readeck/"

// filenameOfVolumeID extracts the kepub file name from a VolumeID/ContentID
// (the path tail), tolerating the chapter "!!" separator and trailing
// slashes.
func filenameOfVolumeID(vid string) string {
	vid = strings.TrimSuffix(vid, "/")
	if i := strings.Index(vid, "!!"); i >= 0 {
		vid = vid[:i]
	}
	if i := strings.LastIndexByte(vid, '/'); i >= 0 {
		vid = vid[i+1:]
	}
	return vid
}
