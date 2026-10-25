package explorer

import (
	"context"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

func ListTargetSchemas(ctx context.Context, target *dbconn.Target) ([]Schema, error) {
	if target.DBMS == dbconn.MariaDB {
		return mariaSchemas(ctx, target)
	}
	return ListSchemas(ctx, target.Postgres)
}

func DescribeTargetTable(ctx context.Context, target *dbconn.Target, schema, table string) (*TableDetail, error) {
	if target.DBMS == dbconn.MariaDB {
		return mariaDescribe(ctx, target, schema, table)
	}
	return DescribeTable(ctx, target.Postgres, schema, table)
}

func ReadTargetData(ctx context.Context, target *dbconn.Target, schema, table string, page int) (*DataPage, error) {
	if target.DBMS == dbconn.MariaDB {
		return mariaData(ctx, target, schema, table, page)
	}
	return ReadData(ctx, target.Postgres, schema, table, page)
}
