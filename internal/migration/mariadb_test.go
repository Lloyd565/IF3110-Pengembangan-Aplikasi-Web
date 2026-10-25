package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/platformdb"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/testdb"
	"github.com/google/uuid"
)

func TestMariaSQLValidation(t *testing.T) {
	for _, q := range []string{
		"CREATE TABLE t(id INT PRIMARY KEY)", "ALTER TABLE t ADD COLUMN title TEXT", "CREATE UNIQUE INDEX idx ON t(id)", "DROP TABLE `t`",
		"INSERT INTO t VALUES ('semi;colon'); UPDATE t SET title='it\\'s ok' WHERE id=1",
		"# comment\n INSERT INTO `semi;colon` VALUES (1); -- comment\n DELETE FROM t WHERE id=2",
		"CREATE VIEW v AS SELECT id FROM t",
		"CREATE TABLE t (engine VARCHAR(20)) ENGINE=InnoDB", "ALTER TABLE t ADD COLUMN engine INT",
	} {
		if _, _, err := mariaSQL(q); err != nil {
			t.Fatalf("valid SQL rejected %q: %v", q, err)
		}
	}
	for _, q := range []string{
		"", "/* only comment */", "CREATE TABLE t(id INT);INSERT INTO t VALUES(1)", "INSERT INTO t VALUES(1);ALTER TABLE t ADD x INT",
		"DROP TABLE a,b", "CREATE OR REPLACE TABLE t(id INT)", "CREATE TABLE t ENGINE=MyISAM AS SELECT 1",
		"ALTER TABLE t ENGINE=MyISAM", "CREATE TABLE t(id INT) ENGINE='MyISAM'", "CREATE TABLE t AS SELECT 1",
		"BEGIN;INSERT INTO t VALUES(1);COMMIT", "SET autocommit=1", "CALL something()", "USE otherdb",
		"/*!40101 SET autocommit=1 */ SELECT 1", "/*M!100100 SET autocommit=1 */", "SELECT 1 INTO OUTFILE 'file'",
		"INSERT INTO t VALUES('unterminated)", "/* unterminated", "DROP DATABASE target",
		"CREATE TABLE t(id INT) ENGINE MyISAM", "ALTER TABLE t ENGINE = `Aria`",
	} {
		if _, _, err := mariaSQL(q); err == nil {
			t.Fatalf("unsafe SQL accepted %q", q)
		}
	}
}

func TestMariaDatabaseScope(t *testing.T) {
	for _, q := range []string{
		"ALTER TABLE `target`.`t` ADD x INT", "SELECT a.id,b.id FROM target.a a JOIN target.b b ON a.id=b.id",
		"SELECT a.id,b.id FROM a,b WHERE a.id=b.id", "SELECT EXTRACT(DAY FROM a.date) FROM target.a a",
		"SELECT * FROM (SELECT a.id FROM target.a a) x", "INSERT INTO target.t VALUES(1.5,'other.database')",
		"SELECT `odd.name` FROM target.t", "CREATE TABLE target.child(id INT, FOREIGN KEY(id) REFERENCES target.parent(id))",
	} {
		if err := checkMariaDatabase(q, "target"); err != nil {
			t.Fatalf("valid scoped SQL %q: %v", q, err)
		}
	}
	for _, q := range []string{
		"SELECT * FROM other.t", "SELECT * FROM `other`.`t`", "SELECT * FROM target.t a,other.t b",
		"SELECT * FROM target.t a JOIN target.t b ON a.id=b.id, other.secret c", "SELECT * FROM (other.secret)",
		"SELECT * FROM (SELECT * FROM other.t) x", "INSERT INTO other.t VALUES(1)", "UPDATE other.t SET x=1",
		"ALTER TABLE other.t ADD x INT", "CREATE INDEX ix ON other.t(x)", "RENAME TABLE target.t TO other.t",
		"CREATE TABLE target.child(id INT, FOREIGN KEY(id) REFERENCES other.parent(id))", "SELECT other.fun()", "SELECT other.t.x FROM target.t",
		"SELECT * FROM target.t STRAIGHT_JOIN mysql.user", "SELECT * FROM 1db.t", "SELECT * FROM t JOIN `1db`.t2 ON 1=1",
	} {
		if err := checkMariaDatabase(q, "target"); err == nil {
			t.Fatalf("cross-database SQL accepted %q", q)
		}
	}
}

func FuzzMariaSQL(f *testing.F) {
	for _, seed := range []string{"SELECT 1", "INSERT INTO t VALUES('a; b')", "/*M! SET autocommit=1 */", "SELECT * FROM (other.t)", "CREATE TABLE `a``b`(id INT)", "'\\"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 100_000 {
			t.Skip()
		}
		stmts, ddl, err := mariaSQL(source)
		if err != nil {
			return
		}
		if len(stmts) == 0 || ddl && len(stmts) != 1 {
			t.Fatal("invalid accepted migration")
		}
		_ = mariaScope(stmts, "target")
	})
}

func TestIntegrationMariaMigrations(t *testing.T) {
	db, name := testdb.FreshMaria(t, "maria_migrations")
	target := &dbconn.Target{DBMS: dbconn.MariaDB, Database: name, Maria: db}
	platform := testdb.Fresh(t, "maria_migration_platform")
	ctx := context.Background()
	if err := platformdb.Migrate(ctx, platform); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := platform.Exec(ctx, `INSERT INTO users(id,username,password_hash) VALUES(1,'budi','unused')`); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Exec(ctx, `INSERT INTO db_connections(id,name,host,port,dbname,username,password_enc,dbms) VALUES($1,'MariaDB','h',3306,'d','u','\x00','mariadb')`, id); err != nil {
		t.Fatal(err)
	}
	create := func(q string) int64 {
		t.Helper()
		mid, err := CreateDraft(ctx, platform, id, 1, "test", q)
		if err != nil {
			t.Fatal(err)
		}
		return mid
	}
	run := func(mid int64) {
		t.Helper()
		if err := ExecuteTarget(ctx, platform, target, id, mid, 1); err != nil {
			t.Fatal(err)
		}
	}
	ddl := create("CREATE TABLE notes(id INT PRIMARY KEY, title VARCHAR(100))")
	run(ddl)
	m, err := Get(ctx, platform, id, ddl)
	if err != nil || m.Status != StatusSuccess || m.Version == nil || *m.Version != 1 {
		t.Fatal("DDL success/history", err)
	}
	if err := ExecuteTarget(ctx, platform, target, id, ddl, 1); !errors.Is(err, ErrNotDraft) {
		t.Fatal("executed migration replay accepted", err)
	}
	batch := create("INSERT INTO notes VALUES(1,'one'); INSERT INTO notes VALUES(2,'two')")
	run(batch)
	fail := create("INSERT INTO notes VALUES(3,'three'); INSERT INTO notes VALUES(1,'duplicate')")
	run(fail)
	m, err = Get(ctx, platform, id, fail)
	if err != nil || m.Status != StatusFailed || m.ErrorMessage == nil {
		t.Fatal("DML failure history", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&count); err != nil || count != 2 {
		t.Fatal("failed batch did not roll back", count, err)
	}
	if _, err := CreateDraft(ctx, platform, id, 1, "bad", "CREATE TABLE partial(id INT); INSERT INTO partial VALUES(1)"); err == nil {
		t.Fatal("mixed DDL accepted")
	}
	failDDL := create("ALTER TABLE notes ADD COLUMN id INT")
	run(failDDL)
	m, err = Get(ctx, platform, id, failDDL)
	if err != nil || m.Status != StatusFailed {
		t.Fatal("DDL failure", err)
	}
	if _, err := ExplainTarget(ctx, target, "CREATE TABLE explain_side_effect(id INT)", true); err == nil {
		t.Fatal("DDL ANALYZE executed")
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME='explain_side_effect'", name).Scan(&count); err != nil || count != 0 {
		t.Fatal("explain modified schema", err)
	}
	for _, analyze := range []bool{false, true} {
		plan, err := ExplainTarget(ctx, target, "SELECT * FROM notes WHERE id=1", analyze)
		if err != nil || len(plan.Statements) != 1 || plan.Statements[0].RawJSON == "" {
			t.Fatal("native explain", err)
		}
	}
	if _, err := ExplainTarget(ctx, target, "UPDATE notes SET title='changed' WHERE id=1", true); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := db.QueryRowContext(ctx, "SELECT title FROM notes WHERE id=1").Scan(&title); err != nil || title != "one" {
		t.Fatal("ANALYZE did not roll back", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE legacy(id INT) ENGINE=MyISAM"); err != nil {
		t.Fatal(err)
	}
	badEngine := create("INSERT INTO notes VALUES(4,'four')")
	run(badEngine)
	m, err = Get(ctx, platform, id, badEngine)
	if err != nil || m.Status != StatusFailed {
		t.Fatal("non-InnoDB database accepted", err)
	}
}
