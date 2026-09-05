// Package manifest describes what an archive contains and where it came from.
//
// The PowerShell scripts recorded nothing, which makes an archive found three
// years later anonymous: there is no way to tell which Azure DevOps project it came
// from, whether its history was truncated, or whether the LFS payload was captured
// completely. A few dozen lines of JSON fix all of that, and let restore point
// origin back at the real remote instead of at a local directory.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"
)

// EntryName is where the manifest lives inside an archive: at the root, as a
// sibling of the bare repository directory rather than inside it, so git never sees
// a stray file and git fsck stays clean.
const EntryName = "clonezip-manifest.json"

// SchemaVersion is incremented when the meaning of a field changes. Readers use it
// to refuse a manifest they cannot interpret rather than guessing.
const SchemaVersion = 1

// LFSScopeTips is the documented scope of every LFS fetch clonezip performs.
const LFSScopeTips = "tips of refs/heads/* and refs/tags/*"

// Manifest is the record written into every archive.
type Manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Tool          string    `json:"tool"`
	CreatedAt     time.Time `json:"createdAt"`

	Source      Source      `json:"source"`
	Clone       Clone       `json:"clone"`
	Archive     Archive     `json:"archive"`
	Refs        []Ref       `json:"refs,omitempty"`
	LFS         LFS         `json:"lfs"`
	Environment Environment `json:"environment"`
	Timings     Timings     `json:"timings"`
}

// Source identifies the repository the archive was made from.
type Source struct {
	// URL never carries credentials. Archives get copied to USB sticks and file
	// shares, so a personal access token pasted into a repo list must not travel
	// with them.
	URL                 string `json:"url"`
	CredentialsRedacted bool   `json:"credentialsRedacted"`

	Host         string `json:"host"`
	Organization string `json:"organization"`
	Project      string `json:"project"`
	Repo         string `json:"repo"`
	HeadRef      string `json:"headRef,omitempty"`

	// Provider names the kind of host: "azure", "github", "codeberg", "bitbucket"
	// or "local".
	// Older archives predate this field and leave it empty.
	Provider string `json:"provider,omitempty"`

	// ListFile and ListLine record which entry of which list produced this
	// archive, which is what disambiguates the repository names that repeat across
	// projects.
	ListFile string `json:"listFile,omitempty"`
	ListLine int    `json:"listLine,omitempty"`
}

// Clone records how the bare repository was fetched.
type Clone struct {
	Bare bool `json:"bare"`

	// Depth is 0 for a full clone. Anything else means history was truncated and
	// cannot be recovered from this archive alone.
	Depth        int  `json:"depth"`
	Shallow      bool `json:"shallow"`
	SingleBranch bool `json:"singleBranch"`
}

// Archive describes the container itself.
//
// There is deliberately no entry count or compressed size here. The manifest has to
// be serialised before it can be written into the archive it describes, so those
// are not knowable yet - and a field that is always zero is worse than one that is
// absent. Both are readable from the archive's own central directory anyway.
type Archive struct {
	// BareDir is the archive's top-level directory. Recorded so restore does not
	// have to infer it, although it can.
	BareDir string `json:"bareDir"`

	// UncompressedBytes is the size of the staged repository, which is what
	// extraction will occupy.
	UncompressedBytes int64 `json:"uncompressedBytes"`
}

// Ref is one branch or tag captured at backup time, so a restore can verify that
// everything present then is present now.
type Ref struct {
	Name string `json:"name"`
	SHA  string `json:"sha,omitempty"`
}

// LFS records what happened during the Git-LFS fetch.
//
// A failed fetch is deliberately not fatal: the archive is still a complete git
// repository, it just leaves LFS-tracked files as pointer stubs. Recording the
// failure is what stops that being a silent surprise years later.
type LFS struct {
	Attempted bool   `json:"attempted"`
	OK        bool   `json:"ok"`
	Warning   string `json:"warning,omitempty"`

	// Scope documents that only branch and tag tips were fetched, not history.
	Scope string `json:"scope,omitempty"`

	ObjectCount int   `json:"objectCount"`
	TotalBytes  int64 `json:"totalBytes"`
	RefBatches  int   `json:"refBatches,omitempty"`
}

// Environment records what produced the archive.
type Environment struct {
	OS       string `json:"os"`
	Git      string `json:"git,omitempty"`
	GitLFS   string `json:"gitLfs,omitempty"`
	CloneZip string `json:"clonezip"`
	Hostname string `json:"hostname,omitempty"`
}

// Timings are per-phase durations in milliseconds, useful for finding which
// repository is responsible for a slow run.
//
// The archive phase is absent for the same reason as the entry count: it has not
// happened yet when this is written. The run log records it.
type Timings struct {
	CloneMs int64 `json:"cloneMs"`
	LFSMs   int64 `json:"lfsMs"`
}

// New returns a manifest with the fields that are the same for every archive
// already filled in.
func New(toolVersion string, createdAt time.Time) *Manifest {
	// A hostname is useful provenance and never essential, so a failure to read it
	// is not worth reporting.
	hostname, _ := os.Hostname()

	return &Manifest{
		SchemaVersion: SchemaVersion,
		Tool:          "clonezip/" + toolVersion,
		CreatedAt:     createdAt,
		Clone:         Clone{Bare: true},
		LFS:           LFS{Scope: LFSScopeTips},
		Environment: Environment{
			OS:       runtime.GOOS + "/" + runtime.GOARCH,
			CloneZip: toolVersion,
			Hostname: hostname,
		},
	}
}

// JSON renders the manifest for storage in an archive. Indented, because the one
// person who ever reads it will be reading it by hand.
func (m *Manifest) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// Parse reads a manifest from archive contents.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", EntryName, err)
	}
	if m.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%s has schema version %d, but this clonezip understands %d: upgrade clonezip",
			EntryName, m.SchemaVersion, SchemaVersion)
	}
	return &m, nil
}

// Truncated reports whether the archive is missing history.
//
// All three signals matter: an explicit depth, git's own shallow marker, and a
// single-branch clone, which loses whole branches rather than old commits.
func (m *Manifest) Truncated() bool {
	return m != nil && (m.Clone.Shallow || m.Clone.Depth > 0 || m.Clone.SingleBranch)
}
