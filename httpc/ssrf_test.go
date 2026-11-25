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
func TestGuardedTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.GuardedTransport(httpc.IsPrivateAddr),
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

// The ranges that are neither RFC1918 nor public, and the IPv6 forms that carry
// an IPv4 destination inside them. Each one was reachable through the guard.
func TestPoliciesCoverTheRangesBetweenPrivateAndPublic(t *testing.T) {
	cases := []struct {
		ip                string
		private, internal bool
	}{
		// CGNAT is a network (a tailnet, a carrier), so only the strict policy
		// refuses it — except the metadata endpoint that lives inside it.
		{"100.64.0.1", true, false},
		{"100.127.255.254", true, false},
		{"100.100.100.200", true, true}, // Alibaba Cloud metadata
		{"100.128.0.1", false, false},   // just past 100.64.0.0/10

		// Never a host on any network.
		{"0.1.2.3", true, true},         // 0.0.0.0/8, "this network"
		{"240.0.0.1", true, true},       // reserved
		{"255.255.255.255", true, true}, // broadcast

		// Metadata endpoints outside link-local.
		{"168.63.129.16", true, true}, // Azure WireServer, a public-looking address
		{"fd00:ec2::254", true, true}, // AWS IMDS over IPv6, inside ULA

		// NAT64 and 6to4 are judged by the IPv4 address they translate to.
		{"64:ff9b::a9fe:a9fe", true, true},    // 169.254.169.254
		{"64:ff9b::7f00:1", true, true},       // 127.0.0.1
		{"64:ff9b::a00:5", true, false},       // 10.0.0.5
		{"64:ff9b::808:808", false, false},    // 8.8.8.8 — DNS64 must keep working
		{"2002:a9fe:a9fe::1", true, true},     // 169.254.169.254
		{"2002:c0a8:101::1", true, false},     // 192.168.1.1
		{"2002:808:808::1", false, false},     // 8.8.8.8
		{"64:ff9b:1::a9fe:a9fe", true, false}, // local-use NAT64 prefix
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.ip)
		if got := httpc.IsPrivateAddr(ip); got != c.private {
			t.Errorf("IsPrivateAddr(%s) = %v, want %v", c.ip, got, c.private)
		}
		if got := httpc.IsInternalAddr(ip); got != c.internal {
			t.Errorf("IsInternalAddr(%s) = %v, want %v", c.ip, got, c.internal)
		}
	}
}

// A guarded fetcher talks to hosts a caller picked. A zero-value Transport
// never closes an idle connection and never times a handshake out, so every
// distinct hostname would cost a file descriptor for the life of the process.
func TestGuardedTransportBoundsItsConnections(t *testing.T) {
	tr := httpc.GuardedTransport(nil)
	if tr.IdleConnTimeout <= 0 {
		t.Error("IdleConnTimeout is unset: idle connections are kept forever")
	}
	if tr.MaxIdleConns <= 0 {
		t.Error("MaxIdleConns is unset: the idle pool is unbounded")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout is unset: a stalled handshake never ends")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("a custom DialContext disables HTTP/2 unless it is forced back on")
	}
	// A proxy would be the address dialled, and the guard would check that instead.
	if tr.Proxy != nil {
		t.Error("the guarded transport must not route through a proxy")
	}
}

// The rule the IPv6 handling must keep: wrapping an IPv4 address in a mapped,
// NAT64 or 6to4 form never makes it more reachable than the address itself.
func FuzzAnEmbeddedIPv4IsJudgedLikeTheAddressItself(f *testing.F) {
	for _, seed := range [][4]byte{{127, 0, 0, 1}, {169, 254, 169, 254}, {10, 0, 0, 5}, {100, 64, 0, 1}, {8, 8, 8, 8}, {0, 0, 0, 0}} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	f.Fuzz(func(t *testing.T, a, b, c, d byte) {
		v4 := netip.AddrFrom4([4]byte{a, b, c, d})
		wrapped := map[string]netip.Addr{
			"mapped": netip.AddrFrom16([16]byte{10: 0xff, 11: 0xff, 12: a, 13: b, 14: c, 15: d}),
			"nat64":  netip.AddrFrom16([16]byte{0: 0x00, 1: 0x64, 2: 0xff, 3: 0x9b, 12: a, 13: b, 14: c, 15: d}),
			"6to4":   netip.AddrFrom16([16]byte{0: 0x20, 1: 0x02, 2: a, 3: b, 4: c, 5: d, 15: 1}),
		}
		for form, ip := range wrapped {
			if got, want := httpc.IsPrivateAddr(ip), httpc.IsPrivateAddr(v4); got != want {
				t.Errorf("IsPrivateAddr(%s %s) = %v, but %s is %v", form, ip, got, v4, want)
			}
			if got, want := httpc.IsInternalAddr(ip), httpc.IsInternalAddr(v4); got != want {
				t.Errorf("IsInternalAddr(%s %s) = %v, but %s is %v", form, ip, got, v4, want)
			}
		}
	})
}
