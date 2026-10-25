package schemadiff

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

// Read captures a repeatable-read, read-only catalog snapshot. Data, row counts,
// sequence counters, owners, comments and grants are outside the comparison.
func Read(ctx context.Context, target *dbconn.Target, schema string) (Snapshot, error) {
	if schema == "" || strings.ContainsRune(schema, 0) {
		return Snapshot{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out Snapshot
	var err error
	switch target.DBMS {
	case dbconn.PostgreSQL:
		if schema == "information_schema" || strings.HasPrefix(schema, "pg_") {
			return out, fmt.Errorf("%w: schema sistem", ErrUnsupported)
		}
		out, err = readPostgres(ctx, target, schema)
	case dbconn.MariaDB:
		if schema != target.Database {
			return out, ErrNotFound
		}
		out, err = readMaria(ctx, target, schema)
	default:
		return out, ErrUnsupported
	}
	if err == nil {
		columns := 0
		for _, table := range out.Tables {
			columns += len(table.Columns)
		}
		if len(out.Tables) > 200 || columns > 2000 {
			return Snapshot{}, fmt.Errorf("%w: maksimal 200 tabel dan 2000 kolom per perbandingan", ErrUnsupported)
		}
	}
	return out, err
}
