package config_test

import (
	"strings"
	"testing"

	"github.com/neodata-io/neokit/config"
)

// An application embeds Base and adds its own fields, so there is one struct and
// one parse. If embedding did not work, every project would need two calls and
// two error paths.
type appConfig struct {
	config.Base
	Issuer string `env:"TEST_ISSUER"`
}

func TestLoadAppliesDefaults(t *testing.T) {
	got, err := config.Load[appConfig]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Port != 8080 {
		t.Errorf("Port = %d, want the 8080 default", got.Port)
	}
	if got.LogLevel != "info" || got.LogFormat != "auto" {
		t.Errorf("log defaults not applied: %+v", got.Base)
	}
	if len(got.CorsOrigins) != 1 || got.CorsOrigins[0] != "http://localhost:3000" {
		t.Errorf("CorsOrigins = %v, want the single localhost default", got.CorsOrigins)
	}
}

func TestLoadReadsTheEnvironmentIncludingEmbeddedFields(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("TEST_ISSUER", "https://id.example.com")
	t.Setenv("CORS_ORIGINS", "https://a.test,https://b.test")
	t.Setenv("BIND_ADDR", "127.0.0.1")

	got, err := config.Load[appConfig]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Port != 9999 {
		t.Errorf("Port = %d, want the environment's 9999", got.Port)
	}
	if got.Issuer != "https://id.example.com" {
		t.Errorf("Issuer = %q — the caller's own fields must parse too", got.Issuer)
	}
	if len(got.CorsOrigins) != 2 {
		t.Errorf("CorsOrigins = %v, want two entries split on the comma", got.CorsOrigins)
	}
	if got.BindAddr != "127.0.0.1" {
		t.Errorf("BindAddr = %q, want the environment's loopback", got.BindAddr)
	}
}

// A malformed value must say which setting and what it choked on. "invalid
// syntax" with neither is the error that costs an hour.
func TestLoadNamesTheOffendingField(t *testing.T) {
	t.Setenv("PORT", "not-a-number")

	_, err := config.Load[appConfig]()
	if err == nil {
		t.Fatal("want an error for an unparseable PORT")
	}
	if !strings.Contains(err.Error(), "Port") {
		t.Errorf("err = %v, want it to name the Port field", err)
	}
	if !strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("err = %v, want it to quote the value it could not parse", err)
	}
}
