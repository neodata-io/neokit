package httpc_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/httpc"
)

func TestIsPrivateAddr(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// The classes an SSRF payload actually aims at.
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"192.168.1.10", true},
		{"172.16.0.1", true},
		{"169.254.169.254", true}, // cloud metadata — the canonical target
		{"fd00::1", true},         // ULA
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback must not slip through Unmap

		// Public addresses a real cover CDN resolves to.
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", c.ip, err)
		}
		if got := httpc.IsPrivateAddr(ip); got != c.want {
			t.Errorf("IsPrivateAddr(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// The guard's whole value is that it blocks at dial time, so exercise it
// through a real client against a real loopback listener rather than asserting
// on the predicate alone.
func TestSSRFGuardedTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.SSRFGuardedTransport(),
		Retry:     &httpc.RetryConfig{},
	})

	resp, err := client.Get(srv.URL) //nolint:bodyclose // err path returns no body
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the guard to refuse a loopback address, got a response")
	}
	if !strings.Contains(err.Error(), "private/internal") {
		t.Errorf("error should name the guard, got: %v", err)
	}
}

func TestSafeAbsoluteArtURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://cdn.example.com/art.jpg", true},
		{"http://cdn.example.com/art.jpg", true},

		{"file:///etc/passwd", false},
		{"gopher://x/", false},
		{"ftp://x/a.jpg", false},
		{"", false},
		{"http:", false},                             // no authority to dial
		{"https://user:pw@cdn.example.com/a", false}, // credentials
		{"/relative/path.jpg", false},                // not absolute
		{"//evil.example.com/a.jpg", false},          // scheme-relative
	}
	for _, c := range cases {
		if got := httpc.SafeAbsoluteURL(c.raw); got != c.want {
			t.Errorf("SafeAbsoluteURL(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestIsInternalAddr(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Blocked even when the LAN is fair game: these are the addresses that
		// mean "this machine" or "this link", never "a box on the network".
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.169.254", true}, // cloud metadata — the canonical target
		{"fe80::1", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback must not slip through Unmap

		// The whole difference from IsPrivateAddr: reaching a box on the home
		// network is the point, not the attack.
		{"10.0.0.5", false},
		{"192.168.1.10", false},
		{"172.16.0.1", false},
		{"fd00::1", false}, // ULA

		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", c.ip, err)
		}
		if got := httpc.IsInternalAddr(ip); got != c.want {
			t.Errorf("IsInternalAddr(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestGuardedTransportRefusesWhatItsPolicyDenies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.GuardedTransport(httpc.IsInternalAddr),
		Retry:     &httpc.RetryConfig{},
	})

	resp, err := client.Get(srv.URL) //nolint:bodyclose // err path returns no body
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the guard to refuse a loopback address, got a response")
	}
	if !strings.Contains(err.Error(), "private/internal") {
		t.Errorf("error should name the guard, got: %v", err)
	}
}

// The policy has to be the injected one, not a hardcoded default. httptest only
// ever binds loopback — the address every stock policy blocks — so a transport
// that ignored its predicate would pass the test above while failing this one.
func TestGuardedTransportConsultsTheInjectedPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("reachable"))
	}))
	defer srv.Close()

	allowEverything := func(netip.Addr) bool { return false }
	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.GuardedTransport(allowEverything),
		Retry:     &httpc.RetryConfig{},
	})

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("a policy that denies nothing must let the dial through: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// A nil policy is the caller forgetting to choose one. Failing open would hand
// them an unguarded client that looks guarded, so it fails closed instead.
func TestGuardedTransportWithNoPolicyFallsBackToTheStrictOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.GuardedTransport(nil),
		Retry:     &httpc.RetryConfig{},
	})

	resp, err := client.Get(srv.URL) //nolint:bodyclose // err path returns no body
	if err == nil {
		resp.Body.Close()
		t.Fatal("a nil policy must not produce an unguarded transport")
	}
}
