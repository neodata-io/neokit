package config_test

import (
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
	if got.MetricsPort != 9090 {
		t.Errorf("MetricsPort = %d, want 9090", got.MetricsPort)
	}
}

func TestLoadReadsTheEnvironmentIncludingEmbeddedFields(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("TEST_ISSUER", "https://id.example.com")
	t.Setenv("CORS_ORIGINS", "https://a.test,https://b.test")

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
}

// A malformed value must name the variable. "strconv.Atoi: parsing \"abc\"" with
// no variable name is the error that costs an hour.
func TestLoadNamesTheOffendingVariable(t *testing.T) {
	t.Setenv("PORT", "not-a-number")

	_, err := config.Load[appConfig]()
	if err == nil {
		t.Fatal("want an error for an unparseable PORT")
	}
	if !contains(err.Error(), "PORT") {
		t.Errorf("err = %v, want it to name PORT", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
