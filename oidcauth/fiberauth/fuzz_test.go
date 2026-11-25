package fiberauth

import (
	"net/url"
	"strings"
	"testing"
)

// Whatever comes in, what goes out is a path on this site: it can never send a
// browser to another origin after login.
func FuzzSafeReturnPath(f *testing.F) {
	for _, seed := range []string{"/", "/a/b?x=1#y", "//evil.example", `/\evil.example`, "https://evil.example", "/%2F%2Fevil", "/\t/evil", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := SafeReturnPath(raw)
		if got != "/" && got != raw {
			t.Fatalf("SafeReturnPath(%q) = %q: neither the input nor the fallback", raw, got)
		}
		if !strings.HasPrefix(got, "/") || strings.HasPrefix(got, "//") || strings.Contains(got, `\`) {
			t.Fatalf("SafeReturnPath(%q) = %q: not a same-site path", raw, got)
		}
		u, err := url.Parse(got)
		if err != nil || u.Scheme != "" || u.Host != "" {
			t.Fatalf("SafeReturnPath(%q) = %q: parses with scheme %q host %q (err %v)", raw, got, u.Scheme, u.Host, err)
		}
	})
}
