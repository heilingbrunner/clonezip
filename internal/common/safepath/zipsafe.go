package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Errors returned when a ZIP entry name may not be turned into a local path.
// They are distinct so tests can assert which rule fired.
var (
	ErrEntryEmpty     = errors.New("empty entry name")
	ErrEntryBackslash = errors.New("entry name contains a backslash")
	ErrEntryAbsolute  = errors.New("entry name is an absolute path")
	ErrEntryEscapes   = errors.New("entry name escapes the destination directory")
)

// EntryError reports a rejected ZIP entry, naming both the entry and the rule.
type EntryError struct {
	Entry string
	Err   error
}

func (e *EntryError) Error() string {
	return fmt.Sprintf("unsafe archive entry %q: %v", e.Entry, e.Err)
}

func (e *EntryError) Unwrap() error { return e.Err }

// SafeJoin resolves a ZIP entry name against an absolute destination directory,
// refusing anything that would write outside it.
//
// This is the zip-slip guard. ZIP entry names are always slash-separated, so a
// backslash is either a Linux-authored traversal attempt or a corrupt archive;
// either way it is refused rather than interpreted. destAbs must already be
// absolute and cleaned - callers get that from filepath.Abs.
//
// A trailing slash, which marks a directory entry, is accepted and dropped: the
// caller decides whether to create a directory or a file from the entry name.
func SafeJoin(destAbs, entryName string) (string, error) {
	fail := func(err error) (string, error) {
		return "", &EntryError{Entry: entryName, Err: err}
	}

	name := strings.TrimSuffix(entryName, "/")
	if name == "" {
		return fail(ErrEntryEmpty)
	}
	if strings.ContainsRune(name, '\\') {
		return fail(ErrEntryBackslash)
	}
	// ZIP entry names are slash-separated, so a leading slash means absolute
	// whatever the host platform thinks. Testing the entry name rather than the
	// converted path matters on Windows, where filepath.IsAbs reports false for
	// a rooted path with no drive letter such as "\etc\passwd".
	if strings.HasPrefix(name, "/") {
		return fail(ErrEntryAbsolute)
	}

	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
		strings.HasPrefix(clean, string(filepath.Separator)) {
		return fail(ErrEntryAbsolute)
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fail(ErrEntryEscapes)
	}

	// Every segment has to survive the same rules as a name derived from the
	// repo list, so an archive cannot smuggle in a reserved device name.
	for seg := range strings.SplitSeq(clean, string(filepath.Separator)) {
		if _, err := SanitizeComponent(seg); err != nil {
			return "", &EntryError{Entry: entryName, Err: err}
		}
	}

	target := filepath.Join(destAbs, clean)

	// Belt and braces: Clean plus the checks above should already guarantee
	// containment, but the joined path is what actually gets written, so verify
	// that rather than trusting the reasoning.
	if target != destAbs && !strings.HasPrefix(target, destAbs+string(filepath.Separator)) {
		return fail(ErrEntryEscapes)
	}
	return target, nil
}

// EntryName converts a path relative to the archive root into a ZIP entry name:
// forward slashes, no leading separator, no volume, no traversal.
func EntryName(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" {
		return "", &EntryError{Entry: rel, Err: ErrEntryAbsolute}
	}
	if clean == "." {
		return "", &EntryError{Entry: rel, Err: ErrEntryEmpty}
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &EntryError{Entry: rel, Err: ErrEntryEscapes}
	}
	return filepath.ToSlash(clean), nil
}
