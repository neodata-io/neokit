package oidcauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// idp is a minimal OpenID provider: discovery, JWKS, and a token endpoint whose
// ID token the test controls claim by claim. It exists so [Provider.Exchange]'s
// validation can be attacked directly — a wrong nonce, a wrong audience, an
// expired token, a token signed by the wrong key.
type idp struct {
	// endSession is advertised as end_session_endpoint when non-empty, so a test
	// can drive both a provider that supports RP-initiated logout and one that
	// does not.
	endSession string

	// tokenFail, when set, answers the token endpoint instead of minting an ID
	// token. It is what lets a test drive the exchange's *rejection* paths as real
	// HTTP answers rather than injected errors, so the RFC 6749 parsing under test
	// is genuinely exercised.
	tokenFail func(w http.ResponseWriter)

	// tokenEndpoint overrides the advertised token_endpoint. Pointing it at a
	// closed port reaches the case production logs could not distinguish:
	// discovery succeeded, but the token request never landed.
	tokenEndpoint string

	key    *rsa.PrivateKey
	url    string
	claims jwt.MapClaims // mutated per test before calling Exchange
	signer *rsa.PrivateKey
}

// mustRSAKey generates a signing key. It panics rather than calling t.Fatal so
// it can also be used where no *testing.T is in scope — the foreign-key forgery
// case needs a second key mid-assertion.
func mustRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generate rsa key: " + err.Error())
	}
	return key
}

func newIDP() *idp {
	key := mustRSAKey()
	return &idp{key: key, signer: key}
}

func (i *idp) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		i.url = base
		w.Header().Set("Content-Type", "application/json")
		doc := map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"jwks_uri":                              base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if i.endSession != "" {
			doc["end_session_endpoint"] = i.endSession
		}
		if i.tokenEndpoint != "" {
			doc["token_endpoint"] = i.tokenEndpoint
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := i.key.Public().(*rsa.PublicKey)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		if i.tokenFail != nil {
			i.tokenFail(w)
			return
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, i.claims)
		tok.Header["kid"] = "test"
		signed, err := tok.SignedString(i.signer)
		if err != nil {
			t.Errorf("sign id_token: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer",
			"expires_in": 3600, "id_token": signed,
		})
	})
	return mux
}

// goodClaims is a valid ID token body; each test spoils exactly one field.
func goodClaims(issuer, aud, nonce string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer, "aud": aud, "sub": "u-1", "nonce": nonce,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"name": "Alex Doe", "email": "alex@example.com",
		"groups": []string{"admins"},
	}
}

// newTestProvider wires a Provider to a live test IdP.
func newTestProvider(t *testing.T, i *idp) *Provider {
	t.Helper()
	srv := httptest.NewServer(i.handler(t))
	t.Cleanup(srv.Close)
	p, ok := New(Config{
		Issuer: srv.URL, ClientID: "app", ClientSecret: "s3cret",
		BaseURL: "https://app.example.com", OwnerGroup: "admins",
	})
	if !ok {
		t.Fatal("New declined a complete config")
	}
	i.url = srv.URL
	return p
}

// rejectWith builds a token endpoint that answers with a given status and body.
func rejectWith(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}
