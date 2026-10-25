package auth

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	password := "kata sandi \u00e9 rahasia"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, password) || !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatal("invalid hash format")
	}
	if !VerifyPassword(hash, password) || VerifyPassword(hash, "wrong") {
		t.Fatal("password verification")
	}
	second, err := HashPassword(password)
	if err != nil || hash == second {
		t.Fatal("salts must be random")
	}
	for _, invalid := range []string{"", "plaintext", strings.Replace(hash, "m=65536", "m=4294967295", 1), strings.Replace(hash, "p=2", "p=256", 1), hash + "$extra"} {
		if VerifyPassword(invalid, password) {
			t.Fatal("malformed hash accepted")
		}
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("empty password accepted")
	}
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordBytes+1)); err == nil {
		t.Fatal("oversized password accepted")
	}
}
