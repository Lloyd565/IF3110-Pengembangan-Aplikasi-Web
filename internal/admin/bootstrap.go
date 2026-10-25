package admin

import (
	"context"
	"strings"
)

// Bootstrap membuat Platform Admin pertama dari konfigurasi (env) saat startup,
// supaya ada akun untuk login pertama kali. Hanya membuat akun jika BELUM ADA
// satu pun admin; kalau sudah ada, tidak melakukan apa-apa.
// Username kosong = bootstrap tidak dikonfigurasi (bukan error).
func (s *Service) Bootstrap(ctx context.Context, username, password string) (created bool, err error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	if err := checkPassword(password); err != nil {
		return false, err
	}
	hash, err := s.Hash(password)
	if err != nil {
		return false, err
	}
	return s.Users.EnsureAdmin(ctx, username, hash)
}
