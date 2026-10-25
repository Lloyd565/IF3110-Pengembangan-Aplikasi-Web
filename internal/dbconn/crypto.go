package dbconn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const keyLen = 32 // AES-256

// ParseKey membaca kunci enkripsi dari teks
func ParseKey(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("dbconn: ENCRYPTION_KEY belum diset")
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("dbconn: ENCRYPTION_KEY bukan base64 yang valid: %w", err)
	}
	if len(key) != keyLen {
		return nil, fmt.Errorf("dbconn: ENCRYPTION_KEY harus %d byte setelah decode base64, dapat %d", keyLen, len(key))
	}
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Encrypt(key []byte, connID uuid.UUID, plain string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), connID[:]), nil
}

func Decrypt(key []byte, connID uuid.UUID, blob []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(blob) < gcm.NonceSize() {
		return "", errors.New("dbconn: data terenkripsi terlalu pendek")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, connID[:])
	if err != nil {
		return "", errors.New("dbconn: gagal mendekripsi password koneksi")
	}
	return string(plain), nil
}
