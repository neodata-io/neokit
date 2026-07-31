package webpush

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// The encodings are a compatibility contract, not an internal detail: a keypair
// is persisted on first boot and every browser subscription is bound to it, so
// the bytes must match what the established Web Push libraries produce.
func TestGenerateKeysProducesTheStandardEncodings(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}

	pub, err := base64.RawURLEncoding.DecodeString(k.Public)
	if err != nil {
		t.Fatalf("public key is not base64url without padding: %v", err)
	}
	if len(pub) != publicKeyLen {
		t.Errorf("public key = %d bytes, want %d (the uncompressed EC point)", len(pub), publicKeyLen)
	}
	if pub[0] != 0x04 {
		t.Errorf("public key starts with 0x%02x, want 0x04 — an uncompressed point", pub[0])
	}

	priv, err := base64.RawURLEncoding.DecodeString(k.Private)
	if err != nil {
		t.Fatalf("private key is not base64url without padding: %v", err)
	}
	if len(priv) != privateKeyLen {
		t.Errorf("private key = %d bytes, want %d (the raw scalar)", len(priv), privateKeyLen)
	}
	// Padding would break every consumer that decodes with RawURLEncoding.
	if strings.ContainsAny(k.Public+k.Private, "=+/") {
		t.Error("keys must be base64url without padding")
	}
}

func TestGeneratedKeysValidate(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}
	if err := k.Validate(); err != nil {
		t.Errorf("a freshly generated pair must validate: %v", err)
	}
}

func TestGenerateKeysAreDistinct(t *testing.T) {
	a, _ := GenerateKeys()
	b, _ := GenerateKeys()
	if a.Private == b.Private || a.Public == b.Public {
		t.Error("two generated keypairs must differ")
	}
}

// The mismatch check is the one worth having: a half-written or hand-edited pair
// produces a server that signs every push with a key the browser will not
// accept — 401 per subscription, per send, forever, with nothing to say why.
func TestValidateRejectsAMismatchedPair(t *testing.T) {
	a, _ := GenerateKeys()
	b, _ := GenerateKeys()

	mixed := Keys{Public: a.Public, Private: b.Private}
	err := mixed.Validate()
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Validate = %v, want ErrInvalidKey", err)
	}
	if !strings.Contains(err.Error(), "derived") {
		t.Errorf("err = %v, want it to name the derivation mismatch", err)
	}
}

func TestValidateRejectsMalformedKeys(t *testing.T) {
	good, _ := GenerateKeys()
	cases := map[string]Keys{
		"empty":              {},
		"public not base64":  {Public: "!!!not base64!!!", Private: good.Private},
		"public wrong size":  {Public: base64.RawURLEncoding.EncodeToString([]byte("short")), Private: good.Private},
		"private not base64": {Public: good.Public, Private: "!!!"},
		"private wrong size": {Public: good.Public, Private: base64.RawURLEncoding.EncodeToString([]byte("short"))},
		"private all zeroes": {Public: good.Public, Private: base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
	}
	for name, k := range cases {
		t.Run(name, func(t *testing.T) {
			if err := k.Validate(); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Validate = %v, want ErrInvalidKey", err)
			}
		})
	}
}

// The prefix must be stripped, because the Go Web Push libraries prepend
// "mailto:" to anything that is not an https URL — handing one a value that
// already carries it produces "mailto:mailto:…", a malformed JWT Apple rejects
// with a bare BadJwtToken and no hint at which claim is wrong.
func TestNormalizeSubjectStripsTheMailtoPrefix(t *testing.T) {
	for _, in := range []string{
		"mailto:ops@example.com",
		"  mailto:ops@example.com  ",
		"ops@example.com",
	} {
		got, err := NormalizeSubject(in)
		if err != nil {
			t.Fatalf("NormalizeSubject(%q): %v", in, err)
		}
		if got != "ops@example.com" {
			t.Errorf("NormalizeSubject(%q) = %q, want the bare address", in, got)
		}
	}
}

func TestNormalizeSubjectKeepsAnHTTPSURL(t *testing.T) {
	const in = "https://example.com/contact"
	got, err := NormalizeSubject(in)
	if err != nil {
		t.Fatalf("NormalizeSubject: %v", err)
	}
	if got != in {
		t.Errorf("NormalizeSubject = %q, want it unchanged", got)
	}
}

// A push service given an unreachable contact may throttle or block the sender
// outright, so a bad value is an error rather than a silent pass-through.
func TestNormalizeSubjectRejectsUnusableContacts(t *testing.T) {
	for name, in := range map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"plain http":  "http://example.com/contact",
		"not an addr": "just some words",
		"bare host":   "example.com",
		"no host":     "https://",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeSubject(in); err == nil {
				t.Errorf("NormalizeSubject(%q) must fail", in)
			}
		})
	}
}

// "use https" is a better answer than "not a valid contact" for the mistake an
// operator actually makes.
func TestNormalizeSubjectNamesTheHTTPMistake(t *testing.T) {
	_, err := NormalizeSubject("http://example.com/contact")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want it to name https", err)
	}
}

func BenchmarkGenerateKeys(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := GenerateKeys(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	k, _ := GenerateKeys()
	b.ReportAllocs()
	for b.Loop() {
		if err := k.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}
