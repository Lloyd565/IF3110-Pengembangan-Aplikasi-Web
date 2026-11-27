package workflow

import (
	"strings"
	"testing"
)

func batchReq(action, sql, down string) request {
	x := request{downSQL: down}
	x.Action, x.Mode, x.BatchSize, x.CursorColumn, x.Scope, x.SQL = action, "batch", 100, "id", "perf.accounts", sql
	return x
}

// Mode batch hanya menerima forward yang bisa dikompensasi (UPDATE dengan manifest baris). DELETE dan
// INSERT ... SELECT lolos validator batch, tetapi checkpoint-nya tanpa manifest sehingga kompensasi
// selalu ditolak setelah migration terlanjur berjalan sebagian.
func TestBatchModeAcceptsOnlyCompensableForwardSQL(t *testing.T) {
	update := `UPDATE perf.accounts SET balance = balance + 1 WHERE id > $1 AND id <= $2`
	undo := `UPDATE perf.accounts SET balance = balance - 1 WHERE id > $1 AND id <= $2`
	cases := []struct {
		name, action, sql, down string
		ok                      bool
	}{
		{"update apply", "apply", update, undo, true},
		{"update forward", "forward", update, undo, true},
		{"update tanpa down", "apply", update, "", true},
		{"delete apply", "apply", `DELETE FROM perf.accounts WHERE id > $1 AND id <= $2`, "", false},
		{"insert select apply", "apply", `INSERT INTO perf.archive SELECT * FROM perf.accounts WHERE id > $1 AND id <= $2`, "", false},
		{"delete forward", "forward", `DELETE FROM perf.accounts WHERE id > $1 AND id <= $2`, "", false},
		// Rollback batch menjalankan SQL down lewat kompensasi; DELETE di arah down tetap didukung.
		{"rollback delete", "rollback", `DELETE FROM perf.accounts WHERE id > $1 AND id <= $2`, "", true},
	}
	for _, c := range cases {
		err := validateBatchRequest(batchReq(c.action, c.sql, c.down))
		if c.ok && err != nil {
			t.Errorf("%s: harus diterima: %v", c.name, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "UPDATE")) {
			t.Errorf("%s: harus ditolak dengan alasan hanya UPDATE: %v", c.name, err)
		}
	}
}
