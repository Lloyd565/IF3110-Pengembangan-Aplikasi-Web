package admin

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_bootstrap")
	if _, err := e.db.Exec(ctx, `UPDATE users SET is_platform_admin = false`); err != nil {
		t.Fatal(err)
	}

	if created, err := e.svc.Bootstrap(ctx, "  ", "apa-saja"); created || err != nil {
		t.Errorf("tidak dikonfigurasi: created=%v err=%v", created, err)
	}
	if _, err := e.svc.Bootstrap(ctx, "owner", "pendek"); !errors.Is(err, ErrInvalid) {
		t.Errorf("password terlalu pendek harus ditolak: %v", err)
	}
	if created, err := e.svc.Bootstrap(ctx, "owner", "password-awal-1"); !created || err != nil {
		t.Fatalf("admin pertama: created=%v err=%v", created, err)
	}

	var admin, active bool
	var hash string
	if err := e.db.QueryRow(ctx,
		`SELECT is_platform_admin, is_active, password_hash FROM users WHERE username = 'owner'`).Scan(&admin, &active, &hash); err != nil {
		t.Fatal(err)
	}
	if !admin || !active || hash == "password-awal-1" {
		t.Errorf("admin=%v active=%v hash=%q", admin, active, hash)
	}

	if created, err := e.svc.Bootstrap(ctx, "owner2", "password-awal-2"); created || err != nil {
		t.Errorf("sudah ada admin: created=%v err=%v", created, err)
	}
}
