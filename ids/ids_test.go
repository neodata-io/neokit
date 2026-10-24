package ids_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/neodata-io/neokit/ids"
)

func TestTokensHaveTheirDocumentedLength(t *testing.T) {
	g := ids.CryptoTokenGenerator{}
	if n := len(g.NewToken()); n != 43 {
		t.Errorf("NewToken length = %d, want 43", n)
	}
	if n := len(g.GeneratePassword()); n != 22 {
		t.Errorf("GeneratePassword length = %d, want 22", n)
	}
	if g.NewToken() == g.NewToken() {
		t.Error("two tokens must differ")
	}
}

func TestUUIDGeneratorProducesV4(t *testing.T) {
	u, err := uuid.Parse(ids.UUIDGenerator{}.New())
	if err != nil {
		t.Fatal(err)
	}
	if u.Version() != 4 {
		t.Errorf("version = %d, want 4", u.Version())
	}
}
