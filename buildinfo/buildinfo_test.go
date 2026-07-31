package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

// The injected values win: they are what a release build stamps in through
// ldflags, and the VCS metadata is only the fallback.
func TestGetPrefersInjectedValues(t *testing.T) {
	got := Get("1.2.3", "abcdef1234567890", "2026-07-31T10:00:00Z")

	if got.Version != "1.2.3" {
		t.Errorf("Version = %q", got.Version)
	}
	if got.Commit != "abcdef1234567890" {
		t.Errorf("Commit = %q", got.Commit)
	}
	if got.Date != "2026-07-31T10:00:00Z" {
		t.Errorf("Date = %q", got.Date)
	}
	if got.GoVersion != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Errorf("runtime identity not filled: %+v", got)
	}
}

// An unversioned build must report an explicit, greppable marker rather than an
// empty string that renders as a blank field in a log line.
func TestGetFallsBackToDevVersion(t *testing.T) {
	if got := Get("", "", "").Version; got != DevVersion {
		t.Errorf("Version = %q, want %q", got, DevVersion)
	}
	if got := Get("   ", "", "").Version; got != DevVersion {
		t.Errorf("a whitespace-only version must fall back too, got %q", got)
	}
}

func TestGetTrimsItsArguments(t *testing.T) {
	got := Get("  1.0.0  ", "  abc  ", "  d  ")
	if got.Version != "1.0.0" || got.Commit != "abc" || got.Date != "d" {
		t.Errorf("values not trimmed: %+v", got)
	}
}

func TestShortCommit(t *testing.T) {
	cases := map[string]string{
		"abcdef1234567890": "abcdef1",
		"abc":              "abc",
		"abcdef1":          "abcdef1",
		"":                 "", // a build with no VCS metadata renders nothing
	}
	for in, want := range cases {
		if got := (Info{Commit: in}).ShortCommit(); got != want {
			t.Errorf("ShortCommit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStringNamesOnlyWhatIsKnown(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want string
	}{
		{"version and commit", Info{Version: "1.2.3", Commit: "abcdef1234"}, "1.2.3 (abcdef1)"},
		{"dirty tree", Info{Version: "1.2.3", Commit: "abcdef1234", Dirty: true}, "1.2.3 (abcdef1, dirty)"},
		{"no commit", Info{Version: "1.2.3"}, "1.2.3"},
		{"no commit but dirty", Info{Version: "dev", Dirty: true}, "dev (dirty)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A `go test` binary is built from a work tree, so Go embeds VCS metadata and
// the blank-argument path should recover a commit. Skipped rather than failed
// where it does not, since a build from a module cache legitimately has none.
func TestGetRecoversVCSMetadataWhenNotInjected(t *testing.T) {
	got := Get("", "", "")
	if got.Commit == "" {
		t.Skip("no VCS metadata embedded in this build")
	}
	if len(got.Commit) < 7 || strings.ContainsAny(got.Commit, " \t") {
		t.Errorf("Commit = %q, want a revision hash", got.Commit)
	}
}

func BenchmarkGet(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = Get("1.2.3", "abcdef1", "2026-07-31")
	}
}
