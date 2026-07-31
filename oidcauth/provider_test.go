package oidcauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/httpc"
)

func TestAuthorizeURLCarriesStateNoncePKCE(t *testing.T) {
	p := newTestProvider(t, newIDP())
	got, err := p.AuthorizeURL(context.Background(), "st4te", "n0nce", "verifier-verifier-verifier-verifier-1234")
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	for _, want := range []string{
		"state=st4te", "nonce=n0nce", "code_challenge=", "code_challenge_method=S256",
		"client_id=app", "redirect_uri=https%3A%2F%2Fapp.example.com%2Fapi%2Fauth%2Fcallback",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("AuthorizeURL missing %q\ngot: %s", want, got)
		}
	}
	// The verifier itself must never travel: sending it instead of its S256
	// challenge would defeat PKCE entirely.
	if strings.Contains(got, "verifier-verifier") {
		t.Error("AuthorizeURL leaked the PKCE verifier")
	}
}

func TestExchangeMapsClaimsToIdentity(t *testing.T) {
	i := newIDP()
	p := newTestProvider(t, i)
	i.claims = goodClaims(i.url, "app", "n0nce")

	id, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "u-1" || id.Name != "Alex Doe" || id.Email != "alex@example.com" {
		t.Errorf("identity = %+v", id)
	}
	if !id.Owner {
		t.Error("member of the configured owner group must be owner")
	}
	if !id.Authenticated() {
		t.Error("a resolved identity must report Authenticated")
	}
}

func TestExchangeOwnerOnlyWhenInGroup(t *testing.T) {
	i := newIDP()
	p := newTestProvider(t, i)
	i.claims = goodClaims(i.url, "app", "n0nce")
	i.claims["groups"] = []string{"guests"}

	id, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Owner {
		t.Error("a non-member must not be owner — this is the whole gate")
	}
}

// A blank OwnerGroup means single-admin deployment: everyone who can sign in is
// an owner. It is the default, so it had better be the documented one.
func TestExchangeBlankOwnerGroupMakesEveryoneOwner(t *testing.T) {
	i := newIDP()
	srv := httptest.NewServer(i.handler(t))
	defer srv.Close()
	i.url = srv.URL
	p, _ := New(Config{Issuer: srv.URL, ClientID: "app", ClientSecret: "s", BaseURL: "https://a.test"})
	i.claims = goodClaims(i.url, "app", "n0nce")
	i.claims["groups"] = []string{"nobody-special"}

	id, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !id.Owner {
		t.Error("with no owner group configured every signed-in user is an owner")
	}
}

// A configurable groups claim is what makes this provider-agnostic: Entra puts
// membership in "roles", and some deployments use a namespaced URI.
func TestExchangeReadsTheConfiguredGroupsClaim(t *testing.T) {
	i := newIDP()
	srv := httptest.NewServer(i.handler(t))
	defer srv.Close()
	i.url = srv.URL
	p, _ := New(Config{
		Issuer: srv.URL, ClientID: "app", ClientSecret: "s", BaseURL: "https://a.test",
		OwnerGroup: "admins", GroupsClaim: "roles",
	})
	i.claims = goodClaims(i.url, "app", "n0nce")
	delete(i.claims, "groups")
	i.claims["roles"] = []string{"admins"}

	id, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !id.Owner || len(id.Groups) != 1 || id.Groups[0] != "admins" {
		t.Errorf("groups claim not read from the configured key: %+v", id)
	}
}

// Providers differ on how they encode a single group. Neither encoding may cost
// a user their access.
func TestExchangeAcceptsBothGroupsEncodings(t *testing.T) {
	for name, groups := range map[string]any{
		"array":       []string{"admins"},
		"bare string": "admins",
	} {
		t.Run(name, func(t *testing.T) {
			i := newIDP()
			p := newTestProvider(t, i)
			i.claims = goodClaims(i.url, "app", "n0nce")
			i.claims["groups"] = groups

			id, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if !id.Owner {
				t.Errorf("group membership lost for the %s encoding: %+v", name, id)
			}
		})
	}
}

// Each of these is a distinct forgery. They are the reason this package exists.
func TestExchangeRejectsBadTokens(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(i *idp)
		nonce string
	}{
		{"replayed nonce", func(i *idp) { i.claims["nonce"] = "someone-elses" }, "n0nce"},
		{"missing nonce claim", func(i *idp) { delete(i.claims, "nonce") }, "n0nce"},
		{"caller lost its nonce", func(i *idp) { i.claims["nonce"] = "" }, ""},
		{"wrong audience", func(i *idp) { i.claims["aud"] = "another-app" }, "n0nce"},
		{"expired", func(i *idp) { i.claims["exp"] = 1 }, "n0nce"},
		{"foreign signing key", func(i *idp) { i.signer = mustRSAKey() }, "n0nce"},
		{"no subject", func(i *idp) { delete(i.claims, "sub") }, "n0nce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := newIDP()
			p := newTestProvider(t, i)
			i.claims = goodClaims(i.url, "app", "n0nce")
			tc.spoil(i) // exactly one field is now a forgery
			if _, err := p.Exchange(context.Background(), "code", tc.nonce, "verifier"); err == nil {
				t.Fatal("Exchange accepted a token it must reject")
			}
		})
	}
}

// Every Exchange failure must be classifiable by identity, not by parsing its
// English text — that is what lets a caller tell the operator *which* thing is
// wrong instead of a flat "login failed".
func TestExchangeFailuresCarryASentinel(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(i *idp)
		nonce string
		want  error
	}{
		{"replayed nonce", func(i *idp) { i.claims["nonce"] = "someone-elses" }, "n0nce", ErrTokenInvalid},
		{"wrong audience", func(i *idp) { i.claims["aud"] = "another-app" }, "n0nce", ErrTokenInvalid},
		{"expired", func(i *idp) { i.claims["exp"] = 1 }, "n0nce", ErrTokenInvalid},
		{"foreign signing key", func(i *idp) { i.signer = mustRSAKey() }, "n0nce", ErrTokenInvalid},
		{"no subject", func(i *idp) { delete(i.claims, "sub") }, "n0nce", ErrTokenInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := newIDP()
			p := newTestProvider(t, i)
			i.claims = goodClaims(i.url, "app", "n0nce")
			tc.spoil(i)
			_, err := p.Exchange(context.Background(), "code", tc.nonce, "verifier")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exchange error = %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

// A failed code exchange has two entirely different fixes, and folding them into
// one sentinel means a rotated client secret and a stale code produce the same
// message — sending someone whose code merely expired off to re-copy a working
// secret. RFC 6749 §5.2 already separates them.
func TestExchangeSeparatesARejectedClientFromARejectedCode(t *testing.T) {
	cases := []struct {
		name    string
		fail    func(w http.ResponseWriter)
		want    error
		notWant error
	}{
		{
			name:    "invalid_client is the secret",
			fail:    rejectWith(http.StatusUnauthorized, `{"error":"invalid_client"}`),
			want:    ErrClientRejected,
			notWant: ErrCodeRejected,
		},
		{
			// Some providers answer invalid_client with 400 rather than 401; the code
			// is the signal, not the status.
			name:    "invalid_client at 400 is still the secret",
			fail:    rejectWith(http.StatusBadRequest, `{"error":"invalid_client"}`),
			want:    ErrClientRejected,
			notWant: ErrCodeRejected,
		},
		{
			// A bare 401 with no RFC body is a credential rejection too — it is the
			// status the spec reserves for exactly that.
			name:    "bare 401 with no error code is the secret",
			fail:    rejectWith(http.StatusUnauthorized, `{}`),
			want:    ErrClientRejected,
			notWant: ErrCodeRejected,
		},
		{
			// The genuinely common one: a code reused (a refreshed callback tab) or
			// left past its short life. Nothing is misconfigured.
			name:    "invalid_grant is the code",
			fail:    rejectWith(http.StatusBadRequest, `{"error":"invalid_grant"}`),
			want:    ErrCodeRejected,
			notWant: ErrClientRejected,
		},
		{
			// An explicit code must win over the status, or a stale code answered
			// with 401 is misreported as a bad secret.
			name:    "invalid_grant at 401 is still the code",
			fail:    rejectWith(http.StatusUnauthorized, `{"error":"invalid_grant"}`),
			want:    ErrCodeRejected,
			notWant: ErrClientRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := newIDP()
			i.tokenFail = tc.fail
			p := newTestProvider(t, i)

			_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exchange error = %v, want it to wrap %v", err, tc.want)
			}
			if errors.Is(err, tc.notWant) {
				t.Errorf("Exchange error = %v, must NOT wrap %v", err, tc.notWant)
			}
		})
	}
}

// A transport failure carries no rejection at all. Reporting it as one is an
// assertion about a request that never arrived.
func TestExchangeUnreachableTokenEndpointDoesNotClaimARejection(t *testing.T) {
	// A port with nothing behind it: started, then immediately closed, so the
	// connection is refused rather than hanging.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	i := newIDP()
	i.tokenEndpoint = deadURL + "/token" // discovery still succeeds; the exchange cannot
	p := newTestProvider(t, i)

	_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err == nil {
		t.Fatal("want an error when the token endpoint refuses the connection")
	}
	if errors.Is(err, ErrCodeRejected) || errors.Is(err, ErrClientRejected) {
		t.Errorf("a request that never landed must not be reported as a rejection: %v", err)
	}
	if !errors.Is(err, ErrTokenEndpointUnreachable) {
		t.Errorf("Exchange error = %v, want ErrTokenEndpointUnreachable", err)
	}
}

// The RFC error code is the one diagnostic worth keeping, and the only part of
// the answer safe to keep: error_description is provider-controlled free text.
func TestExchangeNamesTheRFCCodeWithoutLeakingTheBody(t *testing.T) {
	const secret = "token-endpoint-internal-detail"
	i := newIDP()
	i.tokenFail = rejectWith(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"`+secret+`"}`)
	p := newTestProvider(t, i)

	_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err == nil {
		t.Fatal("want an error from a rejected exchange")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("want the RFC 6749 code in the message so the log is self-diagnosing, got: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Exchange leaked the token-endpoint body: %v", err)
	}
}

// `error` is provider-controlled bytes, not a trusted enum: RFC 6749 constrains
// the *meaning* of the registered codes but the field is free text on the wire.
func TestExchangeDoesNotEchoAnUnregisteredErrorCode(t *testing.T) {
	const injected = "not-a-real-code-with-a-secret-in-it"
	i := newIDP()
	i.tokenFail = rejectWith(http.StatusBadRequest, `{"error":"`+injected+`"}`)
	p := newTestProvider(t, i)

	_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if err == nil {
		t.Fatal("want an error from a rejected exchange")
	}
	if strings.Contains(err.Error(), injected) {
		t.Errorf("Exchange echoed an unregistered error code verbatim: %v", err)
	}
	if !errors.Is(err, ErrCodeRejected) {
		t.Errorf("an unrecognised rejection is still a rejected grant, got: %v", err)
	}
}

// The id_token detail must never reach an operator-facing message: a failed JWKS
// fetch renders as "get keys failed: <status> <body>", so %w-wrapping the verify
// error would put an upstream body into a log line.
func TestExchangeTokenErrorCarriesNoUpstreamDetail(t *testing.T) {
	i := newIDP()
	p := newTestProvider(t, i)
	i.claims = goodClaims(i.url, "app", "n0nce")
	i.claims["aud"] = "another-app"

	_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Exchange error = %v, want ErrTokenInvalid", err)
	}
	// The audience mismatch names the offending value in the library's own
	// message; if that reached us, so would a response body from any other
	// verify failure.
	if strings.Contains(err.Error(), "another-app") {
		t.Errorf("Exchange leaked the verifier's detail: %v", err)
	}
}

// Expiry is the exception worth naming: a container with a drifted clock rejects
// every token it is handed, and nothing else in the chain says so.
func TestExchangeNamesAnExpiredTokenSoTheClockIsSuspected(t *testing.T) {
	i := newIDP()
	p := newTestProvider(t, i)
	i.claims = goodClaims(i.url, "app", "n0nce")
	i.claims["exp"] = 1 // 1970

	_, err := p.Exchange(context.Background(), "code", "n0nce", "verifier")
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Exchange error = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "clock") {
		t.Errorf("an expired id_token must point at the clock, got: %v", err)
	}
}

// A discovery failure must never carry the upstream response body: an issuer
// typo'd onto an internal service would echo that service's body straight back.
func TestDiscoveryErrorDoesNotLeakTheResponseBody(t *testing.T) {
	const secret = "super-secret-internal-payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()

	p, ok := New(Config{Issuer: srv.URL, ClientID: "id", ClientSecret: "sec", BaseURL: "https://a.test"})
	if !ok {
		t.Fatal("New should build with a full config")
	}

	err := p.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("want an error from a 500 discovery response")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("discovery error leaked the upstream body: %v", err)
	}
	if !errors.Is(err, ErrDiscoveryFailed) {
		t.Errorf("want ErrDiscoveryFailed, got: %v", err)
	}
}

// CheckHealth is the lockout guard: it must fail on a wrong client secret and
// pass on a right one. Discovery alone, being unauthenticated, can tell neither
// apart.
func TestCheckHealthVerifiesClientCredentials(t *testing.T) {
	newSrv := func(tokenResp func(w http.ResponseWriter)) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
			base := "http://" + r.Host
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": base, "authorization_endpoint": base + "/authorize",
				"token_endpoint": base + "/token", "jwks_uri": base + "/jwks",
			})
		})
		mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) { tokenResp(w) })
		return httptest.NewServer(mux)
	}

	cases := []struct {
		name      string
		tokenResp func(w http.ResponseWriter)
		wantErr   bool
	}{
		{
			name:      "wrong secret → invalid_client → fail",
			tokenResp: rejectWith(http.StatusUnauthorized, `{"error":"invalid_client"}`),
			wantErr:   true,
		},
		{
			name:      "right secret, bogus code → invalid_grant → pass",
			tokenResp: rejectWith(http.StatusBadRequest, `{"error":"invalid_grant"}`),
			wantErr:   false,
		},
		{
			// A provider answering in some non-standard way must never block a
			// legitimate setup — the probe only fails on an unambiguous rejection.
			name:      "non-standard answer → not disproven → pass",
			tokenResp: rejectWith(http.StatusTeapot, `not json at all`),
			wantErr:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSrv(tc.tokenResp)
			defer srv.Close()
			p, ok := New(Config{Issuer: srv.URL, ClientID: "app", ClientSecret: "sec", BaseURL: "https://a.test"})
			if !ok {
				t.Fatal("New declined a full config")
			}
			err := p.CheckHealth(context.Background())
			if tc.wantErr && err == nil {
				t.Error("want CheckHealth to fail on a rejected client secret")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want CheckHealth to pass, got %v", err)
			}
		})
	}
}

// The context sentinels must survive sanitising: httpc.Classify maps them to
// FaultUnavailable, which is how a caller tells "the network is down" from "your
// configuration is wrong".
func TestDiscoveryErrorPreservesContextSentinels(t *testing.T) {
	for name, sentinel := range map[string]error{
		"canceled": context.Canceled,
		"deadline": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			got := discoveryErr(fmt.Errorf("dial failed: %w", sentinel))
			if !errors.Is(got, sentinel) {
				t.Errorf("discoveryErr(%v) lost the sentinel: %v", sentinel, got)
			}
		})
	}
}

// A container that cannot resolve the issuer host is the most common
// OIDC-in-Docker failure. It must be tagged so a caller can render the fix —
// while staying a net.Error so Classify still reports a deployment problem.
func TestDiscoveryErrorClassifiesUnresolvedIssuer(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "id.example.com", IsNotFound: true}
	got := discoveryErr(&url.Error{
		Op:  "Get",
		URL: "https://id.example.com/.well-known/openid-configuration",
		Err: dnsErr,
	})

	if !errors.Is(got, ErrUnresolved) {
		t.Errorf("want ErrUnresolved, got: %v", got)
	}
	if f := httpc.Classify(got); f != httpc.FaultUnavailable {
		t.Errorf("Classify = %v, want FaultUnavailable — a resolution failure is a deployment problem", f)
	}
}

// A reachable issuer whose TLS certificate does not verify is a distinct,
// actionable failure: "use a trusted certificate", not "unreachable".
func TestDiscoveryErrorClassifiesUntrustedCertificate(t *testing.T) {
	certErr := &tls.CertificateVerificationError{
		Err: errors.New("x509: certificate signed by unknown authority"),
	}
	got := discoveryErr(&url.Error{
		Op:  "Get",
		URL: "https://id.example.com/.well-known/openid-configuration",
		Err: certErr,
	})

	if !errors.Is(got, ErrUntrusted) {
		t.Errorf("want ErrUntrusted, got: %v", got)
	}
}

// RP-initiated logout, and the reason it is worth the round trip: without it,
// signing out drops only the local session while the provider's stays alive, so
// the very next sign-in completes with no prompt. On a shared screen that turns
// "sign out" into a no-op with a reassuring label.
func TestEndSessionURLCarriesClientAndReturn(t *testing.T) {
	i := newIDP()
	i.endSession = "https://id.test/logout"
	p := newTestProvider(t, i)

	got, err := p.EndSessionURL(context.Background())
	if err != nil {
		t.Fatalf("EndSessionURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://id.test/logout" {
		t.Errorf("endpoint = %q, want the advertised end_session_endpoint", got)
	}
	// client_id rather than id_token_hint: both are permitted, and the alternative
	// means storing the raw id_token for the life of every session purely to hand
	// it back — a credential at rest bought for nothing.
	if q := u.Query(); q.Get("client_id") != "app" {
		t.Errorf("client_id = %q, want app", q.Get("client_id"))
	}
	if q := u.Query(); q.Get("post_logout_redirect_uri") != "https://app.example.com/" {
		t.Errorf("post_logout_redirect_uri = %q", q.Get("post_logout_redirect_uri"))
	}
}

// A provider that advertises no end-session endpoint is ordinary, not broken.
func TestEndSessionURLIsEmptyWhenUnsupported(t *testing.T) {
	p := newTestProvider(t, newIDP()) // discovery omits end_session_endpoint
	got, err := p.EndSessionURL(context.Background())
	if err != nil {
		t.Fatalf("an unsupported endpoint must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("EndSessionURL = %q, want empty", got)
	}
}

// Discovery is cached, so a burst of logins on a cold start makes one request
// rather than N. The cache is also what makes Exchange cheap after the first.
func TestDiscoveryIsFetchedOnce(t *testing.T) {
	i := newIDP()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "openid-configuration") {
			hits++
		}
		i.handler(t).ServeHTTP(w, r)
	}))
	defer srv.Close()
	p, _ := New(Config{Issuer: srv.URL, ClientID: "app", ClientSecret: "s", BaseURL: "https://a.test"})

	for range 5 {
		if _, err := p.AuthorizeURL(context.Background(), "s", "n", "v"); err != nil {
			t.Fatalf("AuthorizeURL: %v", err)
		}
	}
	if hits != 1 {
		t.Errorf("discovery fetched %d times, want 1 (the cache is what bounds a cold-start burst)", hits)
	}
}

// CheckHealth deliberately bypasses that cache, so "test connection" reflects the
// network now rather than at first login.
func TestCheckHealthBypassesTheDiscoveryCache(t *testing.T) {
	i := newIDP()
	i.tokenFail = rejectWith(http.StatusBadRequest, `{"error":"invalid_grant"}`)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "openid-configuration") {
			hits++
		}
		i.handler(t).ServeHTTP(w, r)
	}))
	defer srv.Close()
	p, _ := New(Config{Issuer: srv.URL, ClientID: "app", ClientSecret: "s", BaseURL: "https://a.test"})

	for range 3 {
		if err := p.CheckHealth(context.Background()); err != nil {
			t.Fatalf("CheckHealth: %v", err)
		}
	}
	if hits != 3 {
		t.Errorf("discovery fetched %d times, want 3 — CheckHealth must not read a cache", hits)
	}
}

// Concurrent logins on a cold start must not race the discovery cache.
func TestDiscoveryIsRaceFree(t *testing.T) {
	i := newIDP()
	p := newTestProvider(t, i)

	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := p.AuthorizeURL(context.Background(), "s", "n", "v")
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Errorf("concurrent AuthorizeURL: %v", err)
		}
	}
}
