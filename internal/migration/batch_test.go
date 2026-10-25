package migration

import (
	"context"
	"testing"
)

func TestIntegrationDraftBatchAtomic(t *testing.T) {
	db := platformDB(t)
	ctx := context.Background()
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM migrations").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := CreateDraftBatch(ctx, db, connA, 1, "batch", []string{"SELECT 1", "BEGIN"}); err == nil || count() != 0 {
		t.Fatal("validation saved a partial batch")
	}
	if _, err := CreateDraftBatch(ctx, db, connA, 1, "batch", nil); err == nil {
		t.Fatal("empty batch accepted")
	}
	if _, err := db.Exec(ctx, `CREATE FUNCTION reject_second_draft() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name LIKE '[002/%' THEN RAISE EXCEPTION 'injected insert failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_second BEFORE INSERT ON migrations FOR EACH ROW EXECUTE FUNCTION reject_second_draft()`); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateDraftBatch(ctx, db, connA, 1, "batch", []string{"SELECT 1", "SELECT 2"}); err == nil || count() != 0 {
		t.Fatal("database failure saved a partial batch")
	}
	if _, err := db.Exec(ctx, "DROP TRIGGER reject_second ON migrations"); err != nil {
		t.Fatal(err)
	}
	ids, err := CreateDraftBatch(ctx, db, connA, 1, "batch", []string{"SELECT 1", "SELECT 2"})
	if err != nil || len(ids) != 2 || count() != 2 {
		t.Fatal("valid batch", err)
	}
	for i, id := range ids {
		m, err := Get(ctx, db, connA, id)
		if err != nil || m.Status != StatusDraft || m.Version != nil || m.SQL != []string{"SELECT 1", "SELECT 2"}[i] {
			t.Fatal("invalid generated draft", err)
		}
	}
}
