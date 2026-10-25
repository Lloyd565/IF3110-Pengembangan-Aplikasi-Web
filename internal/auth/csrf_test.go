package auth

import (
	"strings"
	"testing"
	"time"
)

func TestCSRF(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := CSRF{Key: make([]byte, 32), Now: func() time.Time { return now }}
	token, err := c.NewPreSession()
	if err != nil || !c.ValidPreSession(token) {
		t.Fatal("valid pre-session rejected", err)
	}
	other := CSRF{Key: []byte(strings.Repeat("x", 32)), Now: c.Now}
	if other.ValidPreSession(token) {
		t.Fatal("forged signature accepted")
	}
	for _, candidate := range []string{"", "1.invalid.invalid", token + "extra", "-9999999999999999999." + token} {
		if c.ValidPreSession(candidate) {
			t.Fatal("malformed token accepted")
		}
	}
	if EqualCSRF("", "") || EqualCSRF("abc", "abd") || !EqualCSRF(token, token) {
		t.Fatal("CSRF comparison")
	}
	now = now.Add(PreSessionTTL)
	if c.ValidPreSession(token) {
		t.Fatal("expired token accepted")
	}
	now = now.Add(-PreSessionTTL - time.Second)
	if c.ValidPreSession(token) {
		t.Fatal("future token accepted")
	}
}
