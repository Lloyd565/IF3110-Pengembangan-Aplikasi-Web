package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type mariaToken struct {
	text         string
	quoted, word bool
}
type mariaStatement struct {
	SQL, Normal string
	Tokens      []mariaToken
}

func splitMaria(source string) ([]mariaStatement, error) {
	var out []mariaStatement
	var normal strings.Builder
	var word strings.Builder
	var tokens []mariaToken
	flushWord := func() {
		if word.Len() > 0 {
			value := word.String()
			allDigits := strings.Trim(value, "0123456789") == ""
			tokens = append(tokens, mariaToken{text: value, word: !allDigits})
			word.Reset()
		}
	}
	start := 0
	emit := func(end int) {
		flushWord()
		value := strings.ToLower(strings.Join(strings.Fields(normal.String()), " "))
		if value != "" {
			out = append(out, mariaStatement{SQL: strings.TrimSpace(source[start:end]), Normal: value, Tokens: tokens})
		}
		normal.Reset()
		tokens = nil
		start = end + 1
	}
	for i := 0; i < len(source); {
		rest := source[i:]
		switch {
		case strings.HasPrefix(rest, "/*"):
			flushWord()
			if strings.HasPrefix(rest, "/*!") || strings.HasPrefix(strings.ToUpper(rest), "/*M!") {
				return nil, ValidationError("MariaDB: executable comment tidak diizinkan.")
			}
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return nil, ValidationError("MariaDB: komentar SQL tidak ditutup.")
			}
			if strings.Contains(rest[2:end+2], "/*") {
				return nil, ValidationError("MariaDB: komentar bersarang tidak didukung.")
			}
			i += end + 4
			normal.WriteByte(' ')
		case rest[0] == '#' || (strings.HasPrefix(rest, "--") && (len(rest) == 2 || rest[2] <= ' ')):
			flushWord()
			end := strings.IndexByte(rest, '\n')
			if end < 0 {
				i = len(source)
			} else {
				i += end + 1
			}
			normal.WriteByte(' ')
		case rest[0] == '\'' || rest[0] == '"' || rest[0] == '`':
			flushWord()
			quote := rest[0]
			i++
			quotedStart := i
			closed := false
			for i < len(source) {
				if source[i] == '\\' && quote != '`' {
					i += 2
					continue
				}
				if source[i] == quote {
					if i+1 < len(source) && source[i+1] == quote {
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, ValidationError("MariaDB: string atau identifier tidak ditutup.")
			}
			if quote == '`' {
				tokens = append(tokens, mariaToken{text: strings.ReplaceAll(source[quotedStart:i-1], "``", "`"), quoted: true, word: true})
			} else {
				tokens = append(tokens, mariaToken{text: "$literal"})
			}
			normal.WriteString(" ident ")
		case rest[0] == ';':
			emit(i)
			i++
		default:
			c := rest[0]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' || c >= 128 {
				word.WriteByte(c)
			} else {
				flushWord()
				if c > ' ' {
					tokens = append(tokens, mariaToken{text: string(c)})
				}
			}
			normal.WriteByte(rest[0])
			i++
		}
	}
	emit(len(source))
	if len(out) == 0 {
		return nil, ValidationError("SQL migration tidak berisi perintah apa pun.")
	}
	return out, nil
}

func mariaScope(stmts []mariaStatement, database string) error {
	type state struct{ query, from, relation bool }
	for _, st := range stmts {
		current := state{query: hasPrefixWord(st.Normal, "delete")}
		var stack []state
		for i, tok := range st.Tokens {
			if tok.text == "(" {
				relation := current.relation
				stack = append(stack, current)
				current = state{relation: relation, from: relation}
				continue
			}
			if tok.text == ")" {
				if len(stack) > 0 {
					current = stack[len(stack)-1]
					stack = stack[:len(stack)-1]
				}
				continue
			}
			if tok.text == "," && current.from {
				current.relation = true
				continue
			}
			if !tok.word {
				continue
			}
			if i+4 < len(st.Tokens) && st.Tokens[i+1].text == "." && st.Tokens[i+2].word && st.Tokens[i+3].text == "." && tok.text != database {
				return ValidationError("MariaDB: referensi ke database lain tidak diizinkan.")
			}
			if i+3 < len(st.Tokens) && st.Tokens[i+1].text == "." && st.Tokens[i+2].word && st.Tokens[i+3].text == "(" && tok.text != database {
				return ValidationError("MariaDB: fungsi di database lain tidak diizinkan.")
			}
			keyword := ""
			if !tok.quoted {
				keyword = strings.ToLower(tok.text)
			}
			switch keyword {
			case "select":
				current.query = true
				current.from, current.relation = false, false
			case "from":
				if current.query {
					current.from = true
					current.relation = true
				}
				continue
			case "join", "straight_join":
				if current.query || hasPrefixWord(st.Normal, "update") {
					current.relation = true
				}
				continue
			case "update":
				if i == 0 {
					current.from = true
					current.relation = true
					continue
				}
			case "insert", "replace":
				if i == 0 {
					current.relation = true
					continue
				}
			case "into", "table", "view", "references":
				current.relation = true
				continue
			case "to":
				if hasPrefixWord(st.Normal, "rename table") || hasPrefixWord(st.Normal, "alter table") {
					current.relation = true
					continue
				}
			case "on":
				if hasPrefixWord(st.Normal, "create index") || hasPrefixWord(st.Normal, "create unique index") || hasPrefixWord(st.Normal, "drop index") {
					current.relation = true
					continue
				}
			case "using":
				if hasPrefixWord(st.Normal, "delete") {
					current.from = true
					current.relation = true
					continue
				}
			case "where", "set", "group", "order", "having", "limit", "union", "values":
				current.from = false
				current.relation = false
			case "if", "not", "exists", "ignore", "low_priority", "high_priority", "delayed":
				if current.relation {
					continue
				}
			}
			if current.relation {
				if i+2 < len(st.Tokens) && st.Tokens[i+1].text == "." && st.Tokens[i+2].word && tok.text != database {
					return ValidationError("MariaDB: objek SQL harus berada di database koneksi.")
				}
				current.relation = false
			}
		}
	}
	return nil
}

func checkMariaDatabase(source, database string) error {
	stmts, _, err := mariaSQL(source)
	if err != nil {
		return err
	}
	return mariaScope(stmts, database)
}

var mariaEngine = regexp.MustCompile(`\bengine\s*(=\s*)?([a-z0-9_]+)`)

var nonInnoDBEngines = map[string]bool{
	"myisam": true, "aria": true, "memory": true, "heap": true, "csv": true, "archive": true,
	"blackhole": true, "federated": true, "federatedx": true, "connect": true, "mroonga": true,
	"rocksdb": true, "spider": true, "s3": true, "sequence": true, "merge": true, "mrg_myisam": true,
	"columnstore": true, "oqgraph": true, "sphinx": true,
}

func mariaSQL(source string) ([]mariaStatement, bool, error) {
	stmts, err := splitMaria(source)
	if err != nil {
		return nil, false, err
	}
	ddl := false
	for _, st := range stmts {
		n := st.Normal
		isDDL := false
		for _, prefix := range []string{"create table", "alter table", "create index", "create unique index", "drop index", "drop table", "rename table", "create view", "alter view", "drop view"} {
			isDDL = isDDL || hasPrefixWord(n, prefix)
		}
		if isDDL {
			ddl = true
			if len(stmts) != 1 {
				return nil, false, ValidationError("MariaDB melakukan implicit commit pada DDL. Gunakan satu statement DDL per migration; jangan campur DDL dengan DML.")
			}
			if (hasPrefixWord(n, "drop table") || hasPrefixWord(n, "drop view") || hasPrefixWord(n, "rename table")) && strings.Contains(n, ",") {
				return nil, false, ValidationError("MariaDB: operasi DROP/RENAME dibatasi satu objek per migration.")
			}
			if hasPrefixWord(n, "create table") && (strings.Contains(" "+n+" ", " select ") || createTableLike(st.Tokens)) {
				return nil, false, ValidationError("MariaDB: CREATE TABLE AS SELECT/LIKE tidak didukung; pisahkan schema dan data.")
			}
			for _, engine := range mariaEngine.FindAllStringSubmatch(n, -1) {
				if (engine[1] != "" && engine[2] != "innodb") || nonInnoDBEngines[engine[2]] {
					return nil, false, ValidationError("MariaDB: hanya engine InnoDB yang didukung.")
				}
			}
			if strings.Contains(" "+n+" ", " partition ") {
				return nil, false, ValidationError("MariaDB: DDL partisi tidak didukung.")
			}
			continue
		}
		allowed := false
		for _, prefix := range []string{"insert", "update", "delete", "replace", "select"} {
			allowed = allowed || hasPrefixWord(n, prefix)
		}
		if !allowed {
			return nil, false, ValidationError("MariaDB: gunakan DML INSERT/UPDATE/DELETE/REPLACE/SELECT atau satu DDL tabel/index/view. Kontrol transaksi, prosedur, dan perintah administrasi tidak diizinkan.")
		}
		if strings.Contains(" "+n+" ", " outfile ") || strings.Contains(" "+n+" ", " dumpfile ") {
			return nil, false, ValidationError("MariaDB: output ke file tidak diizinkan.")
		}
	}
	return stmts, ddl, nil
}

func checkMariaSQL(source string) error { _, _, err := mariaSQL(source); return err }

func createTableLike(tokens []mariaToken) bool {
	keyword := func(i int, word string) bool {
		return i < len(tokens) && !tokens[i].quoted && strings.EqualFold(tokens[i].text, word)
	}
	i := 2
	if keyword(i, "if") && keyword(i+1, "not") && keyword(i+2, "exists") {
		i += 3
	}
	i++
	if i+1 < len(tokens) && tokens[i].text == "." {
		i += 2
	}
	if i < len(tokens) && tokens[i].text == "(" {
		i++
	}
	return keyword(i, "like")
}

func ExecuteTarget(ctx context.Context, platform *pgxpool.Pool, target *dbconn.Target, connID uuid.UUID, id, userID int64) error {
	if target.DBMS == dbconn.PostgreSQL {
		return Execute(ctx, platform, target.Postgres, connID, id, userID)
	}
	return execute(ctx, platform, connID, id, userID, func(source string) error { return checkMariaDatabase(source, target.Database) }, target.Ping, func(ctx context.Context, sql string) error { return runMaria(ctx, target, sql) })
}

func withMaria(ctx context.Context, target *dbconn.Target, fn func(context.Context, *sql.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout+LockTimeout+5*time.Second)
	defer cancel()
	conn, err := target.Maria.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	name := fmt.Sprintf("if3110:%x", sha256.Sum256([]byte(target.Database)))[:64]
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, int64(LockTimeout/time.Second)).Scan(&locked); err != nil {
		return err
	}
	if !locked.Valid || locked.Int64 != 1 {
		return ErrBusy
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var released sql.NullInt64
		if err := conn.QueryRowContext(cleanup, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	for _, setting := range []string{fmt.Sprintf("SET SESSION innodb_lock_wait_timeout=%d", max(1, int64(LockTimeout/time.Second))), fmt.Sprintf("SET SESSION lock_wait_timeout=%d", max(1, int64(LockTimeout/time.Second))), fmt.Sprintf("SET SESSION max_statement_time=%f", StatementTimeout.Seconds())} {
		if _, err := conn.ExecContext(ctx, setting); err != nil {
			return err
		}
	}
	return fn(ctx, conn)
}

func requireInnoDB(ctx context.Context, conn *sql.Conn, database string) error {
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_TYPE='BASE TABLE' AND ENGINE<>'InnoDB'`, database).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ValidationError("MariaDB: transaksi ditolak karena database berisi tabel non-InnoDB.")
	}
	return nil
}

func runMaria(ctx context.Context, target *dbconn.Target, source string) error {
	stmts, ddl, err := mariaSQL(source)
	if err != nil {
		return err
	}
	if err := mariaScope(stmts, target.Database); err != nil {
		return err
	}
	return withMaria(ctx, target, func(ctx context.Context, conn *sql.Conn) error {
		if ddl {
			_, err := conn.ExecContext(ctx, stmts[0].SQL)
			return err
		}
		if err := requireInnoDB(ctx, conn, target.Database); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.SQL); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func ExplainTarget(ctx context.Context, target *dbconn.Target, source string, analyze bool) (*Plan, error) {
	if target.DBMS != dbconn.PostgreSQL {
		return nil, ValidationError("Execution plan hanya tersedia untuk PostgreSQL.")
	}
	return Explain(ctx, target.Postgres, source, analyze)
}
