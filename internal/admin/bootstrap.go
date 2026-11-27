package admin

import (
	"context"
	"strings"
)

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
