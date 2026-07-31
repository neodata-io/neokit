// Package buildinfo reports the identity of the running binary: the version,
// commit and build date baked in at link time, plus the Go toolchain and target
// it was built for.
//
// The values are passed in rather than held here, and that is the whole design.
// `-ldflags -X` names a *package path*, so a variable living in this package
// could only ever be set as `-X github.com/neodata-io/neokit/buildinfo.Version=…`
// — which reads as though it stamps the library, cannot differ between two
// binaries built from one workspace, and silently collides the moment anything
// else in the module graph wants its own version. Keeping the vars in the
// consumer's own package leaves the ldflags path saying what it means:
//
//	go build -ldflags "-X example.com/app/internal/buildinfo.Version=1.2.3"
//
// What is genuinely reusable is the *fallback*: Go embeds VCS metadata into any
// binary built from a work tree, so a plain `go build` or `go run` still knows
// its commit, its build time, and whether the tree was dirty. That logic is
// identical in every project and is what this package exists to hold.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Info is a snapshot of build and runtime identity. The JSON tags are part of
// the contract: it is routinely served from a /version or /health endpoint.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// DevVersion is what Get reports when no version was injected — an explicit,
// greppable marker rather than an empty string, so an unversioned build is
// obvious in a log line instead of rendering as a blank field.
const DevVersion = "dev"

// Get returns build identity, filling any blank argument from the VCS metadata
// Go embeds automatically when building from a work tree.
//
// All three arguments are normally the caller's own ldflags-injected variables.
// Passing "" for each is entirely reasonable — a local build then reports the
// real commit and build time, and DevVersion for the version.
func Get(version, commit, date string) Info {
	info := Info{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		Date:      strings.TrimSpace(date),
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if info.Version == "" {
		info.Version = DevVersion
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		case "vcs.modified":
			// Always read, never overridden: dirtiness is a fact about the tree the
			// binary was built from, and there is no ldflags value to defer to.
			info.Dirty = s.Value == "true"
		}
	}
	return info
}

// ShortCommit is the first 7 characters of the commit, the form a human reads.
// Empty stays empty — a build with no VCS metadata should render nothing rather
// than a misleading placeholder.
func (i Info) ShortCommit() string {
	if len(i.Commit) <= 7 {
		return i.Commit
	}
	return i.Commit[:7]
}

// String renders the identity for a startup banner or a log line, e.g.
// "1.2.3 (a1b2c3d, dirty)". It names only what is known: a build with no commit
// is just its version.
func (i Info) String() string {
	var b strings.Builder
	b.WriteString(i.Version)
	if c := i.ShortCommit(); c != "" {
		b.WriteString(" (")
		b.WriteString(c)
		if i.Dirty {
			b.WriteString(", dirty")
		}
		b.WriteString(")")
	} else if i.Dirty {
		b.WriteString(" (dirty)")
	}
	return b.String()
}
