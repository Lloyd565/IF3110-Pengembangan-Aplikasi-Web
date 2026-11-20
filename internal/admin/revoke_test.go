package admin

import (
	"context"
	"testing"
)

func (e *env) revoked(t *testing.T, userID int64) bool {
	t.Helper()
	var v *bool
	if err := e.db.QueryRow(context.Background(),
		`SELECT bool_and(revoked_at IS NOT NULL) FROM sessions WHERE user_id = $1`, userID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		t.Fatalf("user %d tidak punya session", userID)
	}
	return *v
}

func TestAccessReductionRevokesSessions(t *testing.T) {
	ctx := context.Background()
	e := setup(t, "admin_revoke")
	conn := e.newConn(t, "DB-A")
	gid, err := e.svc.CreateGroup(ctx, e.actor, "tim", "")
	if err != nil {
		t.Fatal(err)
	}
	u1, u2, lain := e.newUser(t, "u1"), e.newUser(t, "u2"), e.newUser(t, "lain")
	for _, u := range []int64{u1, u2} {
		if err := e.svc.AddMember(ctx, e.actor, gid, u); err != nil {
			t.Fatal(err)
		}
	}

	fresh := func(ids ...int64) {
		t.Helper()
		if _, err := e.db.Exec(ctx, `DELETE FROM sessions`); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			e.newSession(t, id)
		}
	}
	check := func(label string, want map[int64]bool) {
		t.Helper()
		for id, w := range want {
			if got := e.revoked(t, id); got != w {
				t.Errorf("%s: session user %d revoked=%v, seharusnya %v", label, id, got, w)
			}
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(e.svc.SetGrant(ctx, e.actor, gid, conn, "view"))
	fresh(u1, u2, lain)
	must(e.svc.SetGrant(ctx, e.actor, gid, conn, "edit"))
	check("level naik", map[int64]bool{u1: false, u2: false, lain: false})
	if !e.forceRotate(t, u1) {
		t.Error("level naik: session harus force_rotate")
	}

	fresh(u1, u2, lain, e.actor)
	must(e.svc.SetGrant(ctx, e.actor, gid, conn, "view"))
	check("level turun", map[int64]bool{u1: true, u2: true, lain: false, e.actor: false})

	fresh(u1, u2)
	must(e.svc.SetGrant(ctx, e.actor, gid, conn, "view"))
	check("level sama", map[int64]bool{u1: false, u2: false})

	fresh(u1, u2, lain)
	must(e.svc.RemoveMember(ctx, e.actor, gid, u1))
	check("keluar group", map[int64]bool{u1: true, u2: false, lain: false})
	if e.can(u1, conn, "schema.view") {
		t.Error("user yang dikeluarkan harus langsung ditolak")
	}

	must(e.svc.AddMember(ctx, e.actor, gid, u1))
	fresh(u1, u2, lain)
	must(e.svc.RemoveGrant(ctx, e.actor, gid, conn))
	check("grant dicabut", map[int64]bool{u1: true, u2: true, lain: false})

	must(e.svc.AddMember(ctx, e.actor, gid, e.actor))
	must(e.svc.SetGrant(ctx, e.actor, gid, conn, "view"))
	fresh(u2, e.actor)
	must(e.svc.RemoveGrant(ctx, e.actor, gid, conn))
	check("admin pelaku", map[int64]bool{u2: true, e.actor: false})
	fresh(e.actor)
	must(e.svc.RemoveMember(ctx, e.actor, gid, e.actor))
	check("admin keluar dari group sendiri", map[int64]bool{e.actor: false})
}
