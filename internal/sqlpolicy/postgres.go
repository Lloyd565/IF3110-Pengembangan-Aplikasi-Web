package sqlpolicy

import (
	"errors"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	parser "github.com/wasilibs/go-pgquery"
)

func Parse(sql string) (*pg.ParseResult, error) {
	if strings.IndexByte(sql, 0) >= 0 {
		return nil, errors.New("SQL mengandung karakter NUL.")
	}
	tree, err := parser.Parse(sql)
	if err != nil {
		return nil, errors.New("Sintaks SQL PostgreSQL tidak valid.")
	}
	if len(tree.Stmts) == 0 {
		return nil, errors.New("SQL migration tidak berisi perintah apa pun.")
	}
	return tree, nil
}

func Check(sql string) error {
	tree, err := Parse(sql)
	if err != nil {
		return err
	}
	tokens, err := parser.Scan(sql)
	if err != nil {
		return errors.New("Sintaks SQL PostgreSQL tidak valid.")
	}
	for _, token := range tokens.Tokens {
		if token.Token == pg.Token_IDENT || token.Token == pg.Token_UIDENT {
			value := strings.Trim(string(sql[token.Start:token.End]), `"`)
			if strings.EqualFold(value, "_platform") || strings.EqualFold(value, "set_config") ||
				strings.HasPrefix(strings.ToLower(value), "pg_advisory_unlock") {
				return errors.New("Objek dan kontrol sesi internal tidak dapat dipakai dalam migration.")
			}
		}
	}
	for _, raw := range tree.Stmts {
		n := raw.Stmt
		if copy := n.GetCopyStmt(); copy != nil && (copy.IsProgram || copy.Filename != "") {
			return errors.New("Migration tidak boleh membuka file atau menjalankan program pada server database.")
		}
		if n.GetTransactionStmt() != nil {
			return errors.New("Migration tidak boleh mengatur transaksi; transaksi dikelola platform.")
		}
		if v := n.GetVariableSetStmt(); v != nil {
			if !v.IsLocal || v.Name == "role" || v.Name == "session_authorization" || v.Name == "search_path" || v.Name == "standard_conforming_strings" || v.Name == "transaction_read_only" || v.Name == "session_replication_role" {
				return errors.New("Migration hanya boleh memakai SET LOCAL yang tidak mengubah kebijakan transaksi.")
			}
		}
		if n.GetVacuumStmt().GetIsVacuumcmd() || n.GetCreatedbStmt() != nil || n.GetDropdbStmt() != nil || n.GetAlterSystemStmt() != nil || n.GetCreateTableSpaceStmt() != nil || n.GetDropTableSpaceStmt() != nil || n.GetDiscardStmt() != nil {
			return errors.New("Perintah tidak dapat dijalankan secara aman dalam transaksi migration.")
		}
		if a := n.GetAlterDatabaseStmt(); a != nil {
			for _, option := range a.Options {
				if option.GetDefElem().GetDefname() == "tablespace" {
					return errors.New("ALTER DATABASE SET TABLESPACE tidak dapat dijalankan dalam transaksi.")
				}
			}
		}
		if c := n.GetClusterStmt(); c != nil && c.Relation == nil {
			return errors.New("CLUSTER seluruh database tidak dapat dijalankan dalam transaksi.")
		}
		if a := n.GetAlterTableStmt(); a != nil {
			for _, cmd := range a.Cmds {
				if cmd.GetAlterTableCmd().GetDef().GetPartitionCmd().GetConcurrent() {
					return errors.New("DETACH PARTITION CONCURRENTLY tidak dapat dijalankan dalam transaksi.")
				}
			}
		}
		if i := n.GetIndexStmt(); i != nil && i.Concurrent {
			return errors.New("CREATE INDEX CONCURRENTLY tidak dapat dijalankan dalam transaksi.")
		}
		if d := n.GetDropStmt(); d != nil && d.Concurrent {
			return errors.New("DROP CONCURRENTLY tidak dapat dijalankan dalam transaksi.")
		}
		if r := n.GetReindexStmt(); r != nil {
			if r.Kind == pg.ReindexObjectType_REINDEX_OBJECT_DATABASE || r.Kind == pg.ReindexObjectType_REINDEX_OBJECT_SYSTEM || r.Kind == pg.ReindexObjectType_REINDEX_OBJECT_SCHEMA {
				return errors.New("REINDEX DATABASE/SYSTEM tidak dapat dijalankan dalam transaksi.")
			}
			for _, p := range r.Params {
				if p.GetDefElem().GetDefname() == "concurrently" && !falseOption(p.GetDefElem().Arg) {
					return errors.New("REINDEX CONCURRENTLY tidak dapat dijalankan dalam transaksi.")
				}
			}
		}
	}
	return nil
}

func Strings(nodes []*pg.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.GetString_().GetSval()
	}
	return out
}

func falseOption(n *pg.Node) bool {
	if n == nil {
		return false
	}
	if v := n.GetBoolean(); v != nil {
		return !v.Boolval
	}
	if v := n.GetInteger(); v != nil {
		return v.Ival == 0
	}
	if v := n.GetString_(); v != nil {
		switch strings.ToLower(v.Sval) {
		case "false", "off", "no", "0":
			return true
		}
	}
	return false
}
