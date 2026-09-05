// Package safepath validates strings that are about to become filesystem
// paths. Repository and project names arrive from a plain text file and are
// spliced into directory names, so they are treated as untrusted input.
package safepath

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxComponentLen bounds a single path segment. Azure DevOps repository names
// are far shorter than this; the limit exists to keep total path length
// predictable on Windows, where git itself does not honour \\?\ paths.
const MaxComponentLen = 100

// reservedWindowsNames are unusable as file or directory names on Windows, with
// or without an extension: NUL.txt is just as reserved as NUL. Rejecting them
// everywhere keeps an archive set created on Linux restorable on Windows.
var reservedWindowsNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// InvalidError reports why a name may not be used as a path component. It
// quotes the offending input so the failure names the list entry at fault.
type InvalidError struct {
	Name   string
	Reason string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("invalid path component %q: %s", e.Name, e.Reason)
}

func invalid(name, reason string) error {
	return &InvalidError{Name: name, Reason: reason}
}

// SanitizeComponent checks that name is usable as a single path component on
// both Windows and Linux and returns it unchanged.
//
// It rejects rather than rewrites. Silently mangling a repository name would
// produce an archive under a name nobody searches for, so a bad entry is
// reported and skipped instead.
//
// Dots, hyphens and underscores are legal and common in Azure DevOps names
// (Contoso.MESlite.OPC-UA, llama.cpp-test, VW7_Configurator); only the leading
// or trailing forms that Windows treats specially are refused.
func SanitizeComponent(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", invalid(name, "not valid UTF-8")
	}
	if strings.TrimSpace(name) == "" {
		return "", invalid(name, "empty")
	}
	if name == "." || name == ".." {
		return "", invalid(name, "refers to a directory")
	}
	if n := utf8.RuneCountInString(name); n > MaxComponentLen {
		return "", invalid(name, fmt.Sprintf("%d characters, limit is %d", n, MaxComponentLen))
	}

	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return "", invalid(name, "contains a path separator")
		case strings.ContainsRune(`<>:"|?*`, r):
			return "", invalid(name, fmt.Sprintf("contains %q, which Windows forbids", r))
		case r < 0x20 || r == 0x7F:
			return "", invalid(name, "contains a control character")
		}
	}

	// Windows silently strips these, so the path created would differ from the
	// one requested - and would then not match on the next run.
	if last, _ := utf8.DecodeLastRuneInString(name); last == '.' || last == ' ' {
		return "", invalid(name, "ends with a dot or space, which Windows strips")
	}

	// A reserved name stays reserved with any extension, so test the stem.
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if reservedWindowsNames[strings.ToUpper(stem)] {
		return "", invalid(name, "reserved device name on Windows")
	}

	return name, nil
}
