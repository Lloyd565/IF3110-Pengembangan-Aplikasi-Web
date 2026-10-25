package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

const PreSessionTTL = 10 * time.Minute

type CSRF struct {
	Key []byte
	Now func() time.Time
}

func (c CSRF) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c CSRF) sign(value string) string {
	mac := hmac.New(sha256.New, c.Key)
	mac.Write([]byte("login-csrf-v1\x00" + value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c CSRF) NewPreSession() (string, error) {
	if len(c.Key) != 32 {
		return "", errors.New("CSRF: key 32 byte wajib diisi")
	}
	nonce, err := NewToken()
	if err != nil {
		return "", err
	}
	value := strconv.FormatInt(c.now().Unix(), 10) + "." + nonce
	return value + "." + c.sign(value), nil
}

func (c CSRF) ValidPreSession(token string) bool {
	if len(c.Key) != 32 {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !ValidToken(parts[1]) {
		return false
	}
	issued, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	age := c.now().Sub(time.Unix(issued, 0))
	return age >= 0 && age < PreSessionTTL && EqualCSRF(parts[2], c.sign(parts[0]+"."+parts[1]))
}

func EqualCSRF(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
