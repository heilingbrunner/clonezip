package repolist

import (
	"bytes"
	"strings"
)

// LegacyEntry is one non-blank, non-comment line of a legacy plain-text repo
// list (the format clonezip used before it moved to YAML), classified as either a
// clone URL or decoration.
//
// This exists only to support "clonezip migrate", which converts an old
// list into the current YAML format. The main Parse/ParseFile path never
// produces or accepts it.
type LegacyEntry struct {
	LineNo int
	Text   string
	IsURL  bool
}

// ParseLegacyText splits a legacy plain-text repo list into classified lines.
// It applies the same rules the old parser used: a UTF-8 BOM and CRLF endings
// are stripped, blank lines and "#" comments are dropped, and a line counts as
// a URL when it starts with "https://", "http://" or "file://".
func ParseLegacyText(data []byte) []LegacyEntry {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	var out []LegacyEntry
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, LegacyEntry{LineNo: lineNo, Text: line, IsURL: isLegacyURL(line)})
	}
	return out
}

func isLegacyURL(line string) bool {
	lower := strings.ToLower(line)
	for _, scheme := range []string{"https://", "http://", "file://"} {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}
