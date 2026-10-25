// Package schemadiff reads catalog snapshots and produces reviewable DDL plans.
// It never executes DDL. Unsupported metadata blocks automatic draft creation.
package schemadiff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

var ErrNotFound = errors.New("schema tidak ditemukan")
var ErrUnsupported = errors.New("schema tidak didukung")

type Snapshot struct {
	DBMS     dbconn.DBMS
	Schema   string
	Tables   []Table
	Incoming []string
}

type Table struct {
	Name                       string
	Columns                    []Column
	Constraints                []Constraint
	Indexes                    []Index
	Engine, Charset, Collation string
	Manual                     []string
}

type Column struct {
	Name, Type, Default, Auto, Charset, Collation, Extra string
	Nullable                                             bool
	Manual                                               string
	Comment                                              string `json:"-"` // Preserve existing MariaDB comments during MODIFY; comments are outside diff scope.
}

type Constraint struct {
	Name, Kind                                       string
	Columns                                          []string
	RefSchema, RefTable                              string // Empty RefSchema means the selected schema itself.
	RefColumns                                       []string
	DeleteColumns                                    []string
	Update, Delete, Match, Check                     string
	Deferrable, Deferred, NotValid, NullsNotDistinct bool
	NoInherit                                        bool
}

type IndexKey struct {
	Name             string
	Prefix           int64
	Desc, NullsFirst bool
}
type Index struct {
	Name, Method             string
	Keys                     []IndexKey
	Include                  []string
	Unique, NullsNotDistinct bool
}

type Change struct {
	Object, Description string
	Destructive         bool
}
type Step struct{ Description, SQL string }
type Plan struct {
	Changes     []Change
	Steps       []Step
	Manual      []string
	Warnings    []string
	Destructive bool
}

func (p Plan) SQL() string {
	var out []string
	for _, step := range p.Steps {
		out = append(out, step.SQL+";")
	}
	return strings.Join(out, "\n\n")
}

func (s Snapshot) Fingerprint() string {
	b, _ := json.Marshal(s)
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

func quote(kind dbconn.DBMS, s string) string {
	if kind == dbconn.MariaDB {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualified(kind dbconn.DBMS, schema, table string) string {
	return quote(kind, schema) + "." + quote(kind, table)
}
func names(kind dbconn.DBMS, values []string) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = quote(kind, value)
	}
	return strings.Join(out, ", ")
}
func same(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (p *Plan) change(object, description string, destructive bool) {
	p.Changes = append(p.Changes, Change{object, description, destructive})
	p.Destructive = p.Destructive || destructive
}
func (p *Plan) step(description, sql string) { p.Steps = append(p.Steps, Step{description, sql}) }
func (p *Plan) manual(object, reason string) {
	p.Manual = append(p.Manual, fmt.Sprintf("%s: %s", object, reason))
}
