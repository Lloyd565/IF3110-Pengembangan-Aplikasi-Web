package dbconn

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncryptDecrypt(t *testing.T) {
	key, id := randKey(t), uuid.New()

	blob, err := Encrypt(key, id, "rahasia-banget")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("rahasia")) {
		t.Fatal("ciphertext tidak boleh memuat plaintext")
	}
	got, err := Decrypt(key, id, blob)
	if err != nil || got != "rahasia-banget" {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	blob2, _ := Encrypt(key, id, "rahasia-banget")
	if bytes.Equal(blob, blob2) {
		t.Error("nonce harus acak: dua enkripsi yang sama tidak boleh identik")
	}

	if _, err := Decrypt(key, uuid.New(), blob); err == nil {
		t.Error("blob milik koneksi lain harus gagal didekripsi (additional data)")
	}
	if _, err := Decrypt(randKey(t), id, blob); err == nil {
		t.Error("kunci yang salah harus gagal")
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := Decrypt(key, id, tampered); err == nil {
		t.Error("data yang diubah harus gagal (integritas GCM)")
	}
	if _, err := Decrypt(key, id, []byte{1, 2, 3}); err == nil {
		t.Error("blob terlalu pendek harus gagal")
	}
}

func TestParseKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(randKey(t))
	if _, err := ParseKey(good); err != nil {
		t.Errorf("kunci valid ditolak: %v", err)
	}
	for name, in := range map[string]string{
		"kosong":       "",
		"bukan base64": "!!!bukan-base64!!!",
		"16 byte":      base64.StdEncoding.EncodeToString(make([]byte, 16)),
	} {
		if _, err := ParseKey(in); err == nil {
			t.Errorf("%s harus ditolak", name)
		}
	}
}
