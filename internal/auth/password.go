package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory   = 64 * 1024
	passwordTime     = 3
	passwordThreads  = 2
	MaxPasswordBytes = 1024
)

var ErrPassword = errors.New("password harus berisi 1 sampai 1024 byte")

func HashPassword(password string) (string, error) {
	if len(password) == 0 || len(password) > MaxPasswordBytes {
		return "", ErrPassword
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordTime, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) bool {
	if len(password) > MaxPasswordBytes || len(encoded) > 512 {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false
	}
	var values [2]uint64
	for i, prefix := range []string{"m=", "t="} {
		if !strings.HasPrefix(params[i], prefix) {
			return false
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(params[i], prefix), 10, 32)
		if err != nil {
			return false
		}
		values[i] = v
	}
	parallelism, ok := strings.CutPrefix(params[2], "p=")
	if !ok {
		return false
	}
	p, err := strconv.ParseUint(parallelism, 10, 8)
	if err != nil || p == 0 || p > 8 {
		return false
	}
	memory, rounds, threads := uint32(values[0]), uint32(values[1]), uint8(p)

	if memory < 8*uint32(threads) || memory > 128*1024 || rounds == 0 || rounds > 6 {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return false
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, rounds, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
