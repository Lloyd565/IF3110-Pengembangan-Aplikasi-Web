package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func TokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }

func ValidToken(token string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(b) == 32 && len(token) == 43
}

func rotatedToken(key []byte, old string, id int64, at time.Time) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "session-rotation-v1\x00%s\x00%d\x00%d", old, id, at.UnixMicro())
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
