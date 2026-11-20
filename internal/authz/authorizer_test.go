package authz

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
)

func mustExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestAuthorizer(t *testing.T) {
	ctx := context.Background()
	db := testdb.Fresh(t, "authz")
	if err := platformdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	// user 1 = anggota g_view + g_edit, user 2 = hanya g_view, user 3 = admin tanpa group,
	// user 4 = nonaktif tapi punya grant edit.
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, is_platform_admin, is_active) VALUES
		(1,'multi','x',false,true), (2,'viewer','x',false,true), (3,'admin','x',true,true), (4,'off','x',false,false)`)
	mustExec(t, db, `INSERT INTO groups (id, name) VALUES (1,'g_view'), (2,'g_edit')`)
	mustExec(t, db, `INSERT INTO group_members (group_id, user_id) VALUES (1,1), (2,1), (1,2), (2,4)`)

	connA, connB, connGone := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, db, `INSERT INTO db_connections (id,name,host,port,dbname,username,password_enc) VALUES
		($1,'A','h',5432,'d','u','\x00'), ($2,'B','h',5432,'d','u','\x00')`, connA, connB)
	mustExec(t, db, `INSERT INTO db_connections (id,name,host,port,dbname,username,password_enc,deleted_at)
		VALUES ($1,'Gone','h',5432,'d','u','\x00', now())`, connGone)
	mustExec(t, db, `INSERT INTO db_grants (group_id, connection_id, level) VALUES
		(1,$1,'view'), (2,$1,'edit'), (1,$3,'view'), (2,$2,'edit')`, connA, connB, connGone)

	az := &Authorizer{DB: db}
	p := func(id int64, admin bool) reqctx.Principal { return reqctx.Principal{UserID: id, IsPlatformAdmin: admin} }

	cases := []struct {
		name string
		who  reqctx.Principal
		conn uuid.UUID
		act  Action
		ok   bool
	}{
		{"akses efektif MAX: view+edit boleh execute", p(1, false), connA, MigrationExecute, true},
		{"view only boleh lihat schema", p(2, false), connA, SchemaView, true},
		{"view only ditolak create migration", p(2, false), connA, MigrationCreate, false},
		{"view only ditolak execute", p(2, false), connA, MigrationExecute, false},
		{"tanpa grant di koneksi lain ditolak", p(2, false), connB, SchemaView, false},
		{"admin tanpa grant ditolak", p(3, true), connA, SchemaView, false},
		{"user nonaktif ditolak", p(4, false), connB, SchemaView, false},
		{"koneksi soft-delete ditolak", p(1, false), connGone, SchemaView, false},
		{"koneksi tidak ada ditolak", p(1, false), uuid.New(), SchemaView, false},
	}
	for _, c := range cases {
		err := az.Can(ctx, c.who, c.conn, c.act)
		if c.ok && err != nil {
			t.Errorf("%s: harusnya boleh, dapat %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: harusnya ErrForbidden, dapat %v", c.name, err)
		}
	}

	t.Run("fail closed saat DB error", func(t *testing.T) {
		bad := testdb.Fresh(t, "authz_bad") // tanpa tabel platform, query pasti error
		err := (&Authorizer{DB: bad}).Can(ctx, p(1, false), connA, SchemaView)
		if err == nil {
			t.Fatal("error pembacaan grant tidak boleh berubah menjadi boleh")
		}
	})

	t.Run("VisibleConnections", func(t *testing.T) {
		got, err := az.VisibleConnections(ctx, p(1, false))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "A" || got[0].Level != LevelEdit || got[1].Name != "B" || got[1].Level != LevelEdit {
			t.Errorf("user 1: %+v", got)
		}
		got, _ = az.VisibleConnections(ctx, p(2, false))
		if len(got) != 1 || got[0].Name != "A" || got[0].Level != LevelView {
			t.Errorf("user 2 (view only, koneksi terhapus tersembunyi): %+v", got)
		}
		if got, _ = az.VisibleConnections(ctx, p(3, true)); len(got) != 0 {
			t.Errorf("admin tanpa grant tidak melihat database apa pun: %+v", got)
		}
	})

	t.Run("VisibleConnections menampilkan group milik sendiri saja", func(t *testing.T) {
		// user 1 anggota g_view dan g_edit; user 2 hanya g_view. Keduanya terhubung ke A lewat grant.
		got, err := az.VisibleConnections(ctx, p(1, false))
		if err != nil || len(got) != 2 {
			t.Fatalf("user 1: %+v, %v", got, err)
		}
		if !slices.Equal(got[0].Groups, []string{"g_edit", "g_view"}) {
			t.Errorf("user 1 di A: groups = %v, mau [g_edit g_view]", got[0].Groups)
		}
		if !slices.Equal(got[1].Groups, []string{"g_edit"}) {
			t.Errorf("user 1 di B: groups = %v, mau [g_edit]", got[1].Groups)
		}

		got, err = az.VisibleConnections(ctx, p(2, false))
		if err != nil || len(got) != 1 {
			t.Fatalf("user 2: %+v, %v", got, err)
		}
		if !slices.Equal(got[0].Groups, []string{"g_view"}) {
			t.Errorf("user 2 hanya boleh melihat group miliknya (g_view), dapat %v: group g_edit milik user lain tidak boleh bocor", got[0].Groups)
		}
	})
}
