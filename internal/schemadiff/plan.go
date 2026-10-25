package schemadiff

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

func Compare(current, desired Snapshot) (Plan, error) {
	var p Plan
	if current.DBMS != desired.DBMS {
		return p, fmt.Errorf("perbandingan harus menggunakan DBMS yang sama")
	}
	if current.DBMS != dbconn.PostgreSQL && current.DBMS != dbconn.MariaDB {
		return p, ErrUnsupported
	}
	kind := current.DBMS
	old := tableMap(current.Tables)
	next := tableMap(desired.Tables)
	all := unionKeys(old, next)
	for _, name := range all {
		a, had := old[name]
		b, want := next[name]
		switch {
		case !had:
			p.change(name, "Tambah tabel", false)
		case !want:
			p.change(name, "Hapus tabel beserta datanya", true)
		default:
			ac, bc := columnMap(a.Columns), columnMap(b.Columns)
			for _, col := range unionKeys(ac, bc) {
				x, had := ac[col]
				y, want := bc[col]
				switch {
				case !had:
					p.change(name+"."+col, "Tambah kolom", false)
					if !y.Nullable && y.Default == "" && y.Auto == "" {
						p.Warnings = append(p.Warnings, name+"."+col+": kolom NOT NULL baru memerlukan default atau pengisian data bila tabel tidak kosong.")
					}
				case !want:
					p.change(name+"."+col, "Hapus kolom beserta datanya", true)
				case !same(x, y):
					p.change(name+"."+col, "Ubah tipe/default/nullability/atribut kolom", x.Type != y.Type)
				}
			}
			for _, c := range diffConstraints(a, b) {
				p.change(name+"."+c, "Ubah constraint", false)
			}
			for _, ix := range diffIndexes(a, b) {
				p.change(name+"."+ix, "Ubah index", false)
			}
			if !same(columnNames(a.Columns), columnNames(b.Columns)) && sameSet(columnNames(a.Columns), columnNames(b.Columns)) {
				p.change(name, "Ubah urutan kolom", false)
			}
			if a.Charset != b.Charset || a.Collation != b.Collation || a.Engine != b.Engine {
				p.change(name, "Ubah opsi tabel", false)
			}
			if !same(a.Manual, b.Manual) {
				p.change(name, "Perubahan objek/atribut yang memerlukan review manual", false)
			}
		}
	}
	if len(p.Changes) == 0 {
		return p, nil
	}
	for _, snapshot := range []Snapshot{current, desired} {
		for _, t := range snapshot.Tables {
			for _, reason := range t.Manual {
				p.manual(t.Name, reason)
			}
			for _, c := range t.Columns {
				if c.Manual != "" {
					p.manual(t.Name+"."+c.Name, c.Manual)
				}
				if c.Auto != "" && c.Nullable {
					p.manual(t.Name+"."+c.Name, "auto-increment nullable memerlukan review manual")
				}
			}
			for _, c := range t.Constraints {
				if c.RefSchema != "" {
					p.manual(t.Name+"."+c.Name, "foreign key mengacu schema di luar cakupan")
				}
				if c.Kind != "PRIMARY KEY" && c.Kind != "UNIQUE" && c.Kind != "FOREIGN KEY" && c.Kind != "CHECK" {
					p.manual(t.Name+"."+c.Name, "jenis constraint tidak didukung")
				}
			}
		}
	}
	if len(current.Incoming) > 0 {
		for _, name := range current.Incoming {
			p.manual(name, "foreign key dari schema lain; dependensi luar cakupan tidak dihapus otomatis")
		}
	}
	affected := affectedForeignKeys(kind, old, next)
	for _, t := range current.Tables {
		for _, c := range t.Constraints {
			if c.Kind == "FOREIGN KEY" && affected[t.Name+"."+c.Name] {
				p.step("Lepas foreign key "+t.Name+"."+c.Name, "ALTER TABLE "+qualified(kind, current.Schema, t.Name)+" "+dropConstraint(kind, c))
			}
		}
	}
	if kind == dbconn.PostgreSQL {
		for _, name := range all {
			a, had := old[name]
			b, want := next[name]
			if !had || !want {
				continue
			}
			bc := constraintMap(b.Constraints)
			bi := indexMap(b.Indexes)
			for _, c := range a.Constraints {
				if c.Kind == "FOREIGN KEY" {
					continue
				}
				if v, ok := bc[c.Name]; !ok || !same(c, v) {
					p.step("Lepas constraint "+name+"."+c.Name, "ALTER TABLE "+qualified(kind, current.Schema, name)+" "+dropConstraint(kind, c))
				}
			}
			for _, ix := range a.Indexes {
				if v, ok := bi[ix.Name]; !ok || !same(ix, v) {
					p.step("Hapus index "+ix.Name, "DROP INDEX "+qualified(kind, current.Schema, ix.Name))
				}
			}
		}
	}
	for _, name := range all {
		if _, want := next[name]; !want {
			p.step("Hapus tabel "+name, "DROP TABLE "+qualified(kind, current.Schema, name))
		}
	}
	// Existing MariaDB tables use one combined ALTER so auto_increment never
	// temporarily loses its required key between separate DDL statements.
	for _, name := range all {
		a, had := old[name]
		b, want := next[name]
		if !had || !want {
			continue
		}
		if kind == dbconn.PostgreSQL {
			alterPostgres(&p, current.Schema, a, b)
		} else {
			alterMaria(&p, current.Schema, a, b)
		}
	}
	for _, name := range all {
		b := next[name]
		if _, had := old[name]; had {
			continue
		}
		if _, want := next[name]; !want {
			continue
		}
		var parts []string
		for _, c := range b.Columns {
			parts = append(parts, columnSQL(kind, c))
		}
		if kind == dbconn.MariaDB {
			for _, c := range b.Constraints {
				if c.Kind != "FOREIGN KEY" {
					parts = append(parts, constraintSQL(kind, current.Schema, c))
				}
			}
			for _, ix := range b.Indexes {
				parts = append(parts, mariaIndex(ix))
			}
		}
		q := "CREATE TABLE " + qualified(kind, current.Schema, name) + " (\n  " + strings.Join(parts, ",\n  ") + "\n)"
		if kind == dbconn.MariaDB {
			q += " ENGINE=InnoDB DEFAULT CHARACTER SET " + quote(kind, b.Charset) + " COLLATE " + quote(kind, b.Collation)
		}
		p.step("Buat tabel "+name, q)
	}
	if kind == dbconn.PostgreSQL {
		for _, name := range all {
			b, want := next[name]
			if !want {
				continue
			}
			a, had := old[name]
			ac := constraintMap(a.Constraints)
			ai := indexMap(a.Indexes)
			for _, c := range b.Constraints {
				if c.Kind == "FOREIGN KEY" {
					continue
				}
				if v, ok := ac[c.Name]; !had || !ok || !same(c, v) {
					p.step("Tambah constraint "+name+"."+c.Name, "ALTER TABLE "+qualified(kind, current.Schema, name)+" ADD "+constraintSQL(kind, current.Schema, c))
				}
			}
			for _, ix := range b.Indexes {
				if v, ok := ai[ix.Name]; !had || !ok || !same(ix, v) {
					q := "CREATE "
					if ix.Unique {
						q += "UNIQUE "
					}
					q += "INDEX " + quote(kind, ix.Name) + " ON " + qualified(kind, current.Schema, name) + " USING btree (" + indexKeys(kind, ix) + ")"
					if len(ix.Include) > 0 {
						q += " INCLUDE (" + names(kind, ix.Include) + ")"
					}
					if ix.NullsNotDistinct {
						q += " NULLS NOT DISTINCT"
					}
					p.step("Buat index "+ix.Name, q)
				}
			}
		}
	}
	for _, t := range desired.Tables {
		existing := constraintMap(old[t.Name].Constraints)
		for _, c := range t.Constraints {
			if prev, ok := existing[c.Name]; c.Kind == "FOREIGN KEY" && (affected[t.Name+"."+c.Name] || !ok || !same(prev, c)) {
				p.step("Pasang foreign key "+t.Name+"."+c.Name, "ALTER TABLE "+qualified(kind, current.Schema, t.Name)+" ADD "+constraintSQL(kind, current.Schema, c))
			}
		}
	}
	if len(p.Steps) > 250 {
		p.manual("plan", "maksimal 250 statement; bandingkan schema yang lebih kecil")
	}
	sort.Strings(p.Manual)
	p.Manual = dedup(p.Manual)
	if len(p.Manual) > 0 {
		p.Steps = nil
	}
	return p, nil
}

func alterPostgres(p *Plan, schema string, a, b Table) {
	kind := dbconn.PostgreSQL
	ac, bc := columnMap(a.Columns), columnMap(b.Columns)
	table := qualified(kind, schema, a.Name)
	for _, c := range a.Columns {
		if _, ok := bc[c.Name]; !ok {
			p.step("Hapus kolom "+a.Name+"."+c.Name, "ALTER TABLE "+table+" DROP COLUMN "+quote(kind, c.Name))
		}
	}
	for _, c := range b.Columns {
		old, had := ac[c.Name]
		if !had {
			p.step("Tambah kolom "+a.Name+"."+c.Name, "ALTER TABLE "+table+" ADD COLUMN "+columnSQL(kind, c))
			continue
		}
		if old.Auto != c.Auto || (old.Type != c.Type && (old.Auto != "" || c.Auto != "")) {
			p.manual(a.Name+"."+c.Name, "perubahan identity/sequence memerlukan review manual")
			continue
		}
		prefix := "ALTER TABLE " + table + " ALTER COLUMN " + quote(kind, c.Name) + " "
		if old.Type != c.Type {
			if old.Default != "" {
				p.step("Lepas default "+c.Name, prefix+"DROP DEFAULT")
			}
			p.step("Ubah tipe "+c.Name, prefix+"TYPE "+c.Type)
			p.Warnings = append(p.Warnings, a.Name+"."+c.Name+": periksa cast dan data yang sudah ada sebelum mengeksekusi perubahan tipe.")
		}
		if old.Default != c.Default || old.Type != c.Type {
			if c.Default != "" {
				p.step("Ubah default "+c.Name, prefix+"SET DEFAULT "+c.Default)
			} else if old.Default != "" && old.Type == c.Type {
				p.step("Hapus default "+c.Name, prefix+"DROP DEFAULT")
			}
		}
		if old.Nullable != c.Nullable {
			action := "SET NOT NULL"
			if c.Nullable {
				action = "DROP NOT NULL"
			}
			p.step("Ubah nullability "+c.Name, prefix+action)
		}
	}
	var resulting []string
	for _, c := range a.Columns {
		if _, ok := bc[c.Name]; ok {
			resulting = append(resulting, c.Name)
		}
	}
	for _, c := range b.Columns {
		if _, ok := ac[c.Name]; !ok {
			resulting = append(resulting, c.Name)
		}
	}
	if !same(resulting, columnNames(b.Columns)) {
		p.manual(a.Name, "PostgreSQL tidak mengubah urutan kolom secara in-place; perlu rebuild tabel yang ditinjau manual")
	}
}

func alterMaria(p *Plan, schema string, a, b Table) {
	kind := dbconn.MariaDB
	ac, bc := columnMap(a.Columns), columnMap(b.Columns)
	as, bs := constraintMap(a.Constraints), constraintMap(b.Constraints)
	ai, bi := indexMap(a.Indexes), indexMap(b.Indexes)
	var actions []string
	for _, c := range a.Constraints {
		if c.Kind == "FOREIGN KEY" {
			continue
		}
		if v, ok := bs[c.Name]; !ok || !same(c, v) {
			actions = append(actions, dropConstraint(kind, c))
		}
	}
	for _, ix := range a.Indexes {
		if v, ok := bi[ix.Name]; !ok || !same(ix, v) {
			actions = append(actions, "DROP INDEX "+quote(kind, ix.Name))
		}
	}
	for _, c := range a.Columns {
		if _, ok := bc[c.Name]; !ok {
			actions = append(actions, "DROP COLUMN "+quote(kind, c.Name))
		}
	}
	orderChanged := relativeOrderChanged(a, b)
	for i, c := range b.Columns {
		old, had := ac[c.Name]
		if !had || !same(old, c) || orderChanged {
			if had {
				c.Comment = old.Comment
			}
			action := "ADD COLUMN "
			if had {
				action = "MODIFY COLUMN "
			}
			position := " FIRST"
			if i > 0 {
				position = " AFTER " + quote(kind, b.Columns[i-1].Name)
			}
			actions = append(actions, action+columnSQL(kind, c)+position)
		}
	}
	for _, c := range b.Constraints {
		if c.Kind == "FOREIGN KEY" {
			continue
		}
		if v, ok := as[c.Name]; !ok || !same(c, v) {
			actions = append(actions, "ADD "+constraintSQL(kind, schema, c))
		}
	}
	for _, ix := range b.Indexes {
		if v, ok := ai[ix.Name]; !ok || !same(ix, v) {
			actions = append(actions, "ADD "+mariaIndex(ix))
		}
	}
	if a.Charset != b.Charset || a.Collation != b.Collation {
		actions = append(actions, "DEFAULT CHARACTER SET "+quote(kind, b.Charset)+" COLLATE "+quote(kind, b.Collation))
	}
	if len(actions) > 0 {
		p.step("Ubah tabel "+a.Name, "ALTER TABLE "+qualified(kind, schema, a.Name)+"\n  "+strings.Join(actions, ",\n  "))
	}
}

func affectedForeignKeys(kind dbconn.DBMS, old, next map[string]Table) map[string]bool {
	columnChanged := func(table, column string) bool {
		a, had := old[table]
		b, want := next[table]
		if !had || !want || (kind == dbconn.MariaDB && relativeOrderChanged(a, b)) {
			return true
		}
		x, had := columnMap(a.Columns)[column]
		y, want := columnMap(b.Columns)[column]
		return !had || !want || !same(x, y)
	}
	keysChanged := func(table string, columns []string) bool {
		a, b := old[table], next[table]
		ac, bc := constraintMap(a.Constraints), constraintMap(b.Constraints)
		for _, name := range diffConstraints(a, b) {
			for _, c := range []Constraint{ac[name], bc[name]} {
				if c.Kind == "PRIMARY KEY" || c.Kind == "UNIQUE" {
					for _, col := range c.Columns {
						if slices.Contains(columns, col) {
							return true
						}
					}
				}
			}
		}
		ai, bi := indexMap(a.Indexes), indexMap(b.Indexes)
		for _, name := range diffIndexes(a, b) {
			for _, ix := range []Index{ai[name], bi[name]} {
				for _, key := range ix.Keys {
					if slices.Contains(columns, key.Name) {
						return true
					}
				}
			}
		}
		return false
	}
	out := map[string]bool{}
	for _, t := range old {
		desired := constraintMap(next[t.Name].Constraints)
		for _, c := range t.Constraints {
			if c.Kind != "FOREIGN KEY" {
				continue
			}
			v, ok := desired[c.Name]
			hit := !ok || !same(c, v) || keysChanged(c.RefTable, c.RefColumns) || (kind == dbconn.MariaDB && keysChanged(t.Name, c.Columns))
			for _, col := range c.Columns {
				hit = hit || columnChanged(t.Name, col)
			}
			for _, col := range c.RefColumns {
				hit = hit || columnChanged(c.RefTable, col)
			}
			if hit {
				out[t.Name+"."+c.Name] = true
			}
		}
	}
	return out
}

func relativeOrderChanged(a, b Table) bool {
	ac, bc := columnMap(a.Columns), columnMap(b.Columns)
	var x, y []string
	for _, c := range a.Columns {
		if _, ok := bc[c.Name]; ok {
			x = append(x, c.Name)
		}
	}
	for _, c := range b.Columns {
		if _, ok := ac[c.Name]; ok {
			y = append(y, c.Name)
		}
	}
	return !same(x, y)
}

func columnSQL(kind dbconn.DBMS, c Column) string {
	typeName := c.Type
	if kind == dbconn.PostgreSQL && c.Auto == "serial" {
		typeName = map[string]string{"smallint": "SMALLSERIAL", "integer": "SERIAL", "bigint": "BIGSERIAL"}[c.Type]
	}
	q := quote(kind, c.Name) + " " + typeName
	if kind == dbconn.PostgreSQL {
		switch c.Auto {
		case "always":
			q += " GENERATED ALWAYS AS IDENTITY"
		case "default":
			q += " GENERATED BY DEFAULT AS IDENTITY"
		}
	} else {
		if c.Charset != "" {
			q += " CHARACTER SET " + quote(kind, c.Charset)
		}
		if c.Collation != "" {
			q += " COLLATE " + quote(kind, c.Collation)
		}
	}
	if !c.Nullable {
		q += " NOT NULL"
	} else if kind == dbconn.MariaDB {
		q += " NULL"
	}
	if c.Default != "" && c.Auto == "" {
		q += " DEFAULT " + c.Default
	}
	if kind == dbconn.MariaDB {
		if c.Auto != "" {
			q += " AUTO_INCREMENT"
		}
		if c.Extra != "" {
			q += " " + c.Extra
		}
		if c.Comment != "" {
			q += " COMMENT " + mariaLiteral(c.Comment)
		}
	}
	return q
}

func dropConstraint(kind dbconn.DBMS, c Constraint) string {
	if kind == dbconn.MariaDB {
		switch c.Kind {
		case "PRIMARY KEY":
			return "DROP PRIMARY KEY"
		case "UNIQUE":
			return "DROP INDEX " + quote(kind, c.Name)
		case "FOREIGN KEY":
			return "DROP FOREIGN KEY " + quote(kind, c.Name)
		}
	}
	return "DROP CONSTRAINT " + quote(kind, c.Name)
}

func constraintSQL(kind dbconn.DBMS, schema string, c Constraint) string {
	prefix := "CONSTRAINT " + quote(kind, c.Name) + " "
	if kind == dbconn.MariaDB && c.Kind == "PRIMARY KEY" {
		prefix = ""
	}
	q := prefix + c.Kind
	switch c.Kind {
	case "PRIMARY KEY", "UNIQUE":
		if c.NullsNotDistinct && kind == dbconn.PostgreSQL {
			q += " NULLS NOT DISTINCT"
		}
		q += " (" + names(kind, c.Columns) + ")"
	case "CHECK":
		q += " (" + c.Check + ")"
		if kind == dbconn.PostgreSQL && c.NoInherit {
			q += " NO INHERIT"
		}
	case "FOREIGN KEY":
		ref := schema
		if c.RefSchema != "" {
			ref = c.RefSchema
		}
		q += " (" + names(kind, c.Columns) + ") REFERENCES " + qualified(kind, ref, c.RefTable) + " (" + names(kind, c.RefColumns) + ")"
		if c.Match != "" {
			q += " MATCH " + c.Match
		}
		if c.Update != "" {
			q += " ON UPDATE " + c.Update
		}
		if c.Delete != "" {
			q += " ON DELETE " + c.Delete
			if kind == dbconn.PostgreSQL && len(c.DeleteColumns) > 0 {
				q += " (" + names(kind, c.DeleteColumns) + ")"
			}
		}
	}
	if kind == dbconn.PostgreSQL {
		if c.Deferrable {
			q += " DEFERRABLE"
			if c.Deferred {
				q += " INITIALLY DEFERRED"
			}
		}
		if c.NotValid {
			q += " NOT VALID"
		}
	}
	return q
}

func mariaLiteral(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "\x00", "\\0"), "'", "''") + "'"
}

func indexKeys(kind dbconn.DBMS, ix Index) string {
	var parts []string
	for _, key := range ix.Keys {
		q := quote(kind, key.Name)
		if key.Prefix > 0 {
			q += fmt.Sprintf("(%d)", key.Prefix)
		}
		if key.Desc {
			q += " DESC"
		}
		if kind == dbconn.PostgreSQL {
			if key.NullsFirst {
				q += " NULLS FIRST"
			} else {
				q += " NULLS LAST"
			}
		}
		parts = append(parts, q)
	}
	return strings.Join(parts, ", ")
}
func mariaIndex(ix Index) string {
	prefix := "INDEX "
	if ix.Unique {
		prefix = "UNIQUE INDEX "
	}
	return prefix + quote(dbconn.MariaDB, ix.Name) + " USING BTREE (" + indexKeys(dbconn.MariaDB, ix) + ")"
}

func tableMap(items []Table) map[string]Table {
	out := map[string]Table{}
	for _, v := range items {
		out[v.Name] = v
	}
	return out
}
func columnMap(items []Column) map[string]Column {
	out := map[string]Column{}
	for _, v := range items {
		out[v.Name] = v
	}
	return out
}
func constraintMap(items []Constraint) map[string]Constraint {
	out := map[string]Constraint{}
	for _, v := range items {
		out[v.Name] = v
	}
	return out
}
func indexMap(items []Index) map[string]Index {
	out := map[string]Index{}
	for _, v := range items {
		out[v.Name] = v
	}
	return out
}
func columnNames(items []Column) []string {
	var out []string
	for _, c := range items {
		out = append(out, c.Name)
	}
	return out
}
func unionKeys[T any](a, b map[string]T) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func sameSet(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return same(aa, bb)
}
func diffConstraints(a, b Table) []string {
	old, next := constraintMap(a.Constraints), constraintMap(b.Constraints)
	var out []string
	for _, k := range unionKeys(old, next) {
		x, had := old[k]
		y, want := next[k]
		if !had || !want || !same(x, y) {
			out = append(out, k)
		}
	}
	return out
}
func diffIndexes(a, b Table) []string {
	old, next := indexMap(a.Indexes), indexMap(b.Indexes)
	var out []string
	for _, k := range unionKeys(old, next) {
		x, had := old[k]
		y, want := next[k]
		if !had || !want || !same(x, y) {
			out = append(out, k)
		}
	}
	return out
}
func dedup(items []string) []string {
	var out []string
	for _, s := range items {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
