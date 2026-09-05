package safepath

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeComponentAcceptsRealRepositoryNames(t *testing.T) {
	// Every one of these is a real project or repository name from the committed
	// repo lists. Dots, multiple dots and hyphens after a dot are normal and
	// must not be mistaken for anything special.
	names := []string{
		"bubble",
		"llama.cpp-test",
		"node.ts",
		"golang.org",
		"asp.net-core",
		"Project.CNBM",
		"Contoso.MESlite.OPC-UA",
		"Contoso.LogConsole.Node.js",
		"VW7_Configurator",
		"VisiWin7-Licenses",
		"hp42s_free42_perl",
		"CSharp-Application-Library-101",
	}
	for _, name := range names {
		got, err := SanitizeComponent(name)
		if err != nil {
			t.Errorf("SanitizeComponent(%q) = error %v, want accepted", name, err)
			continue
		}
		if got != name {
			t.Errorf("SanitizeComponent(%q) = %q, want it returned unchanged", name, got)
		}
	}
}

func TestSanitizeComponentRejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // substring of the expected reason
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   ", "empty"},
		{"current dir", ".", "directory"},
		{"parent dir", "..", "directory"},
		{"forward slash", "a/b", "separator"},
		{"backslash", `a\b`, "separator"},
		{"colon", "C:", "Windows forbids"},
		{"asterisk", "bad*name", "Windows forbids"},
		{"question mark", "what?", "Windows forbids"},
		{"pipe", "a|b", "Windows forbids"},
		{"quote", `a"b`, "Windows forbids"},
		{"angle bracket", "a<b", "Windows forbids"},
		{"newline", "a\nb", "control character"},
		{"tab", "a\tb", "control character"},
		{"del", "a\x7fb", "control character"},
		{"trailing dot", "name.", "strips"},
		{"trailing space", "name ", "strips"},
		{"reserved bare", "NUL", "reserved"},
		{"reserved lowercase", "nul", "reserved"},
		{"reserved mixed case", "Con", "reserved"},
		{"reserved with extension", "NUL.txt", "reserved"},
		{"reserved serial port", "COM1", "reserved"},
		{"reserved printer port", "LPT9", "reserved"},
		{"too long", strings.Repeat("a", MaxComponentLen+1), "limit is"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SanitizeComponent(tc.input)
			if err == nil {
				t.Fatalf("SanitizeComponent(%q) = nil error, want rejection", tc.input)
			}
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("error is %T, want *InvalidError", err)
			}
			if !strings.Contains(invalidErr.Reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", invalidErr.Reason, tc.want)
			}
		})
	}
}

func TestSanitizeComponentAllowsLengthLimitExactly(t *testing.T) {
	atLimit := strings.Repeat("a", MaxComponentLen)
	if _, err := SanitizeComponent(atLimit); err != nil {
		t.Errorf("a name of exactly %d characters was rejected: %v", MaxComponentLen, err)
	}
}

func TestSafeJoinAcceptsArchiveEntries(t *testing.T) {
	dest := mustAbs(t, filepath.Join("testdata", "dest"))

	// The shapes an clonezip archive actually contains: the bare repo root, nested
	// object directories, the LFS payload and the manifest sibling.
	entries := []string{
		"MyRepo.git/",
		"MyRepo.git/HEAD",
		"MyRepo.git/objects/pack/pack-0123456789abcdef.pack",
		"MyRepo.git/lfs/objects/ab/cd/abcdef0123456789",
		"MyRepo.git/hooks/pre-commit.sample",
		"clonezip-manifest.json",
	}
	for _, entry := range entries {
		got, err := SafeJoin(dest, entry)
		if err != nil {
			t.Errorf("SafeJoin(dest, %q) = error %v, want accepted", entry, err)
			continue
		}
		if got != dest && !strings.HasPrefix(got, dest+string(filepath.Separator)) {
			t.Errorf("SafeJoin(dest, %q) = %q, which is outside %q", entry, got, dest)
		}
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	dest := mustAbs(t, filepath.Join("testdata", "dest"))

	tests := []struct {
		name  string
		entry string
		want  error
	}{
		{"empty", "", ErrEntryEmpty},
		{"slash only", "/", ErrEntryEmpty},
		{"current dir", ".", ErrEntryEscapes},
		{"parent", "..", ErrEntryEscapes},
		{"parent prefix", "../evil", ErrEntryEscapes},
		{"parent nested", "a/../../evil", ErrEntryEscapes},
		{"absolute unix", "/etc/passwd", ErrEntryAbsolute},
		{"backslash traversal", `..\evil`, ErrEntryBackslash},
		{"backslash separator", `a\b`, ErrEntryBackslash},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeJoin(dest, tc.entry)
			if err == nil {
				t.Fatalf("SafeJoin(dest, %q) = %q, want rejection", tc.entry, got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("SafeJoin(dest, %q) error = %v, want %v", tc.entry, err, tc.want)
			}
			var entryErr *EntryError
			if !errors.As(err, &entryErr) {
				t.Fatalf("error is %T, want *EntryError", err)
			}
			if entryErr.Entry != tc.entry {
				t.Errorf("EntryError.Entry = %q, want %q", entryErr.Entry, tc.entry)
			}
		})
	}
}

func TestSafeJoinRejectsReservedSegment(t *testing.T) {
	dest := mustAbs(t, filepath.Join("testdata", "dest"))

	// An archive must not be able to smuggle in a name that is unusable on
	// Windows; the per-segment check is what catches it.
	if got, err := SafeJoin(dest, "MyRepo.git/NUL/HEAD"); err == nil {
		t.Fatalf("SafeJoin accepted a reserved device name in a segment, returning %q", got)
	}
}

func TestSafeJoinRejectsWindowsAbsolutePath(t *testing.T) {
	dest := mustAbs(t, filepath.Join("testdata", "dest"))
	// This contains a backslash, so it is refused before the volume check ever
	// runs; either rejection is correct, the point is that it never resolves.
	if got, err := SafeJoin(dest, `C:\Windows\System32\evil`); err == nil {
		t.Fatalf("SafeJoin accepted an absolute Windows path, returning %q", got)
	}
}

func TestEntryName(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want string
	}{
		{"plain file", "HEAD", "HEAD"},
		{"nested", filepath.Join("objects", "info", "packs"), "objects/info/packs"},
		{"redundant segments", filepath.Join("a", ".", "b"), "a/b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EntryName(tc.rel)
			if err != nil {
				t.Fatalf("EntryName(%q) = error %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("EntryName(%q) = %q, want %q", tc.rel, got, tc.want)
			}
			if strings.Contains(got, `\`) {
				t.Errorf("EntryName(%q) = %q, which contains a backslash; ZIP names use forward slashes",
					tc.rel, got)
			}
		})
	}
}

func TestEntryNameRejectsEscapes(t *testing.T) {
	for _, rel := range []string{".", "..", filepath.Join("..", "evil")} {
		if got, err := EntryName(rel); err == nil {
			t.Errorf("EntryName(%q) = %q, want rejection", rel, got)
		}
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", path, err)
	}
	return abs
}
