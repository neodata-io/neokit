package httpc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// otherHost returns srv's URL under a different hostname for the same listener,
// so a redirect to it is cross-host without needing a second machine.
func otherHost(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
}

// redirector answers every request with a redirect to target.
func redirector(t *testing.T, target string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// headerRecorder stores the headers of the last request it served.
func headerRecorder(t *testing.T) (*httptest.Server, *atomic.Pointer[http.Header], *atomic.Int32) {
	t.Helper()
	var seen atomic.Pointer[http.Header]
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h := r.Header.Clone()
		seen.Store(&h)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &hits
}

// net/http strips only Authorization and Cookie across hosts. An upstream that
// answers with a redirect would otherwise be handed the API key in a custom
// header — the one HeaderAuth exists to set.
func TestACrossHostRedirectDropsCredentialHeaders(t *testing.T) {
	target, seen, _ := headerRecorder(t)
	up := redirector(t, otherHost(target)+"/elsewhere", http.StatusFound)

	bc := NewBaseClient(up.URL, "svc", func(r *http.Request) {
		r.Header.Set("X-Api-Key", "SECRET")
		r.Header.Set("Authorization", "Bearer TOKEN")
		r.Header.Set("Cookie", "session=abc")
		r.Header.Set("User-Agent", "neokit-test")
		r.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	})
	var out map[string]any
	if err := bc.DoJSON(t.Context(), http.MethodGet, bc.URL("/a?api_key=IN-THE-QUERY"), nil, &out); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	got := *seen.Load()
	for _, h := range []string{"X-Api-Key", "Authorization", "Cookie", "Referer"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s = %q followed the redirect to another host", h, v)
		}
	}
	for h, want := range map[string]string{
		"Accept":      "application/json",
		"User-Agent":  "neokit-test",
		"Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	} {
		if v := got.Get(h); v != want {
			t.Errorf("%s = %q, want the harmless header kept as %q", h, v, want)
		}
	}
}

func TestASameHostRedirectKeepsItsHeaders(t *testing.T) {
	var seen atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		h := r.Header.Clone()
		seen.Store(&h)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "svc", HeaderAuth("X-Api-Key", "SECRET"))
	if err := bc.DoJSON(t.Context(), http.MethodGet, bc.URL("/old"), nil, nil); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if got := seen.Load().Get("X-Api-Key"); got != "SECRET" {
		t.Errorf("X-Api-Key = %q, want it kept on a same-host redirect", got)
	}
}

// A 307/308 replays the request body. Across hosts that hands the payload —
// credentials in a login body included — to whoever the upstream named.
func TestACrossHostRedirectWithABodyIsRefused(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		target, _, hits := headerRecorder(t)
		up := redirector(t, otherHost(target)+"/elsewhere", status)

		bc := NewBaseClient(up.URL, "svc", HeaderAuth("X-Api-Key", "SECRET"))
		err := bc.DoJSON(t.Context(), http.MethodPost, bc.URL("/login"), map[string]string{"password": "hunter2"}, nil)
		if err == nil {
			t.Errorf("status %d: want the redirect refused", status)
		}
		if hits.Load() != 0 {
			t.Errorf("status %d: the redirect target received the replayed request", status)
		}
		if f := Classify(err); f.Retryable() {
			t.Errorf("status %d: Classify = %q, want a refusal that is not retried", status, f)
		}
	}
}

func TestBothClientConstructorsCarryTheRedirectPolicy(t *testing.T) {
	if NewHTTPClient(HTTPOptions{}).CheckRedirect == nil {
		t.Error("NewHTTPClient follows redirects with net/http's default policy")
	}
	if NewWebSocketHTTPClient(nil).CheckRedirect == nil {
		t.Error("NewWebSocketHTTPClient follows redirects with net/http's default policy")
	}
}

func TestSameHost(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"https://api.example/a", "https://api.example/b", true},
		{"https://api.example/a", "https://API.Example:443/b", true},
		{"http://api.example/a", "https://api.example/b", true}, // the TLS upgrade
		{"https://api.example/a", "http://api.example/b", false},
		{"https://api.example/a", "https://evil.example/b", false},
		{"https://api.example/a", "https://sub.api.example/b", false},
		// Another port is another service, which need not share the credential.
		{"http://nas.lan:8096/a", "http://nas.lan:9000/b", false},
		{"http://nas.lan:8096/a", "https://nas.lan/b", false},
	}
	for _, c := range cases {
		from, _ := url.Parse(c.from)
		to, _ := url.Parse(c.to)
		if got := sameHost(from, to); got != c.want {
			t.Errorf("sameHost(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestRedirectsStillStopAfterTen(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	resp, err := NewHTTPClient(HTTPOptions{Retry: &RetryConfig{}}).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect loop must end in an error")
	}
	if hits.Load() != 10 {
		t.Errorf("requests = %d, want net/http's limit of 10", hits.Load())
	}
}
