// Package datacat reads the parts of a project database the data API
// serves (V4 §3): relations, columns, keys, foreign keys and functions in
// the exposed schemas. xenibase-edge caches it per project; xenibase-server
// uses it for type generation and the security advisor.
package datacat

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Relation kinds.
const (
	KindTable    = 'r'
	KindPart     = 'p'
	KindView     = 'v'
	KindMatView  = 'm'
	KindForeign  = 'f'
	maxRelations = 5000
)

// Column is one column.
type Column struct {
	Name     string
	Type     string // format_type, for casts
	Category byte   // pg_type.typcategory: N numeric, S string, B bool, D date/time, U user (uuid, json…), A array…
	TypName  string // pg_type.typname: int4, jsonb, tsvector…
	Nullable bool
	Num      int16
	// HasDefault: a default or an identity (optional on insert);
	// Generated: computed, never written.
	HasDefault bool
	Generated  bool
	// Enum is an enum type's labels.
	Enum []string
}

// JSON reports whether the column is json or jsonb.
func (c *Column) JSON() bool { return c.TypName == "json" || c.TypName == "jsonb" }

// ForeignKey is a foreign key from Table's Cols to Ref's RefCols.
type ForeignKey struct {
	Name    string
	Table   *Table
	Cols    []string
	Ref     *Table
	RefCols []string
}

// Table is a table, view, materialized view or foreign table.
type Table struct {
	Schema   string
	Name     string
	Kind     byte
	RLS      bool // row-level security enabled (tables)
	Invoker  bool // security_invoker (views)
	Columns  []*Column
	ByName   map[string]*Column
	PK       []string
	Out      []*ForeignKey // this table's foreign keys
	In       []*ForeignKey // foreign keys that reference this table
	Estimate float64       // reltuples
}

// Col is the named column, or nil.
func (t *Table) Col(name string) *Column { return t.ByName[name] }

// Qualified is the quoted schema.table.
func (t *Table) Qualified() string {
	return pgx.Identifier{t.Schema, t.Name}.Sanitize()
}

// Querier is a connection or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Arg is a function argument.
type Arg struct {
	Name       string
	Type       string
	Category   byte
	TypName    string
	HasDefault bool
}

// Function is a function in an exposed schema the data API can call (V4
// §3.4). Overloads are separate Functions with the same name. An
// extension's functions are left out: the API serves what the project
// wrote, and an extension that keeps a function from PUBLIC
// (pg_stat_statements_reset) means it.
type Function struct {
	Schema   string
	Name     string
	Args     []Arg // IN and INOUT arguments, in order
	Returns  string
	RetSet   bool
	Volatile byte // i immutable, s stable, v volatile
	Definer  bool // SECURITY DEFINER
	// Columns are a set-returning function's output columns (RETURNS
	// TABLE, OUT arguments, or SETOF a table); nil for a scalar result.
	Columns []*Column
	// RetType and RetCategory describe a scalar result.
	RetType     string
	RetCategory byte
	RetTypName  string
}

// Catalog is a project's exposed relations.
type Catalog struct {
	// Functions by "schema.name" (overloads together).
	Functions   map[string][]*Function
	Schemas     []string
	Fingerprint string
	ByName      map[string]*Table // "schema.name"
	Ordered     []*Table
}

// Find resolves a table name in a URL: "schema.table", or "table" in the
// first exposed schema that has it.
func (c *Catalog) Find(name string) *Table {
	if t := c.ByName[name]; t != nil && strings.Contains(name, ".") {
		return t
	}
	for _, s := range c.Schemas {
		if t := c.ByName[s+"."+name]; t != nil {
			return t
		}
	}
	return nil
}

// FindFunction resolves a function name in a URL like Find.
func (c *Catalog) FindFunction(name string) []*Function {
	if fs := c.Functions[name]; len(fs) > 0 && strings.Contains(name, ".") {
		return fs
	}
	for _, s := range c.Schemas {
		if fs := c.Functions[s+"."+name]; len(fs) > 0 {
			return fs
		}
	}
	return nil
}

// Tables lists the relations in catalog order.
func (c *Catalog) Tables() []*Table { return c.Ordered }

// FingerprintSQL changes when the relations, columns or constraints of
// the schemas in $1 change.
const FingerprintSQL = `SELECT
  (SELECT coalesce(sum(c.xmin::text::bigint), 0) + count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = ANY($1))::text || ':' ||
  (SELECT coalesce(sum(a.xmin::text::bigint), 0) + count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = ANY($1) AND a.attnum > 0)::text || ':' ||
  (SELECT coalesce(sum(k.xmin::text::bigint), 0) + count(*) FROM pg_constraint k JOIN pg_namespace n ON n.oid = k.connamespace
    WHERE n.nspname = ANY($1))::text || ':' ||
  (SELECT coalesce(sum(f.xmin::text::bigint), 0) + count(*) FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace
    WHERE n.nspname = ANY($1))::text`

// Introspect reads the relations of schemas (and their functions).
func Introspect(ctx context.Context, tx Querier, schemas []string) (*Catalog, error) {
	cat := &Catalog{Schemas: schemas, ByName: map[string]*Table{}, Functions: map[string][]*Function{}}
	rows, err := tx.Query(ctx, `SELECT c.oid, n.nspname, c.relname, c.relkind::text, c.relrowsecurity,
		  coalesce(c.reloptions::text[] && ARRAY['security_invoker=true', 'security_invoker=on', 'security_invoker=1'], false),
		  greatest(c.reltuples, 0)::float8
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND NOT c.relispartition
		ORDER BY n.nspname, c.relname LIMIT $2`, schemas, maxRelations)
	if err != nil {
		return nil, err
	}
	byOID := map[uint32]*Table{}
	var oids []uint32
	for rows.Next() {
		var oid uint32
		var kind string
		t := &Table{ByName: map[string]*Column{}}
		if err := rows.Scan(&oid, &t.Schema, &t.Name, &kind, &t.RLS, &t.Invoker, &t.Estimate); err != nil {
			rows.Close()
			return nil, err
		}
		t.Kind = kind[0]
		byOID[oid] = t
		oids = append(oids, oid)
		cat.ByName[t.Schema+"."+t.Name] = t
		cat.Ordered = append(cat.Ordered, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(oids) == 0 { // no relations, but maybe functions
		return cat, introspectFunctions(ctx, tx, cat, byOID)
	}
	rows, err = tx.Query(ctx, `SELECT a.attrelid, a.attname, format_type(a.atttypid, a.atttypmod), t.typcategory::text, t.typname,
		  NOT a.attnotnull, a.attnum, a.atthasdef OR a.attidentity <> '', a.attgenerated <> '',
		  CASE WHEN t.typtype = 'e' THEN (SELECT array_agg(enumlabel::text ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = t.oid) END
		FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return nil, err
	}
	nums := map[uint32]map[int16]string{}
	for rows.Next() {
		var oid uint32
		var cat string
		c := &Column{}
		if err := rows.Scan(&oid, &c.Name, &c.Type, &cat, &c.TypName, &c.Nullable, &c.Num, &c.HasDefault, &c.Generated, &c.Enum); err != nil {
			rows.Close()
			return nil, err
		}
		c.Category = cat[0]
		t := byOID[oid]
		t.Columns = append(t.Columns, c)
		t.ByName[c.Name] = c
		if nums[oid] == nil {
			nums[oid] = map[int16]string{}
		}
		nums[oid][c.Num] = c.Name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT conname, contype::text, conrelid, conkey, confrelid, confkey FROM pg_constraint
		WHERE contype IN ('p', 'f') AND conrelid = ANY($1) ORDER BY conrelid, conname`, oids)
	if err != nil {
		return nil, err
	}
	names := func(oid uint32, keys []int16) []string {
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, nums[oid][k])
		}
		return out
	}
	for rows.Next() {
		var name, typ string
		var rel, frel uint32
		var keys, fkeys []int16
		if err := rows.Scan(&name, &typ, &rel, &keys, &frel, &fkeys); err != nil {
			rows.Close()
			return nil, err
		}
		t := byOID[rel]
		switch typ {
		case "p":
			t.PK = names(rel, keys)
		case "f":
			ref := byOID[frel]
			if ref == nil {
				continue // references a table outside the exposed schemas
			}
			fk := &ForeignKey{Name: name, Table: t, Cols: names(rel, keys), Ref: ref, RefCols: names(frel, fkeys)}
			t.Out = append(t.Out, fk)
			ref.In = append(ref.In, fk)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cat, introspectFunctions(ctx, tx, cat, byOID)
}

func introspectFunctions(ctx context.Context, tx Querier, cat *Catalog, byOID map[uint32]*Table) error {
	rows, err := tx.Query(ctx, `SELECT n.nspname, p.proname, p.proretset, p.provolatile::text, p.prosecdef,
		  coalesce(p.proargnames, '{}'), coalesce(p.proargmodes::text[], '{}'),
		  coalesce((SELECT array_agg(format_type(t, NULL) ORDER BY o) FROM unnest(coalesce(p.proallargtypes, p.proargtypes::oid[])) WITH ORDINALITY AS a(t, o)), '{}'),
		  coalesce((SELECT array_agg(ty.typcategory::text ORDER BY o) FROM unnest(coalesce(p.proallargtypes, p.proargtypes::oid[])) WITH ORDINALITY AS a(t, o) JOIN pg_type ty ON ty.oid = a.t), '{}'),
		  coalesce((SELECT array_agg(ty.typname::text ORDER BY o) FROM unnest(coalesce(p.proallargtypes, p.proargtypes::oid[])) WITH ORDINALITY AS a(t, o) JOIN pg_type ty ON ty.oid = a.t), '{}'),
		  p.pronargdefaults, format_type(p.prorettype, NULL), rt.typcategory::text, rt.typname, rt.typrelid
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace JOIN pg_type rt ON rt.oid = p.prorettype
		WHERE n.nspname = ANY($1) AND p.prokind = 'f' AND rt.typname NOT IN ('trigger', 'event_trigger', 'internal')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		ORDER BY n.nspname, p.proname, p.oid LIMIT 5000`, cat.Schemas)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			f                       Function
			vol, retCat, retTypName string
			names, modes, types     []string
			cats, typnames          []string
			ndefaults               int16
			relid                   uint32
		)
		if err := rows.Scan(&f.Schema, &f.Name, &f.RetSet, &vol, &f.Definer, &names, &modes, &types, &cats, &typnames,
			&ndefaults, &f.RetType, &retCat, &retTypName, &relid); err != nil {
			return err
		}
		f.Volatile, f.RetCategory, f.RetTypName = vol[0], retCat[0], retTypName
		var out []*Column
		for i, t := range types {
			mode := "i"
			if i < len(modes) {
				mode = modes[i]
			}
			name := ""
			if i < len(names) {
				name = names[i]
			}
			c := byte('X')
			if i < len(cats) && cats[i] != "" {
				c = cats[i][0]
			}
			tn := ""
			if i < len(typnames) {
				tn = typnames[i]
			}
			switch mode {
			case "i", "b":
				f.Args = append(f.Args, Arg{Name: name, Type: t, Category: c, TypName: tn})
			case "v":
				f.Args = append(f.Args, Arg{Name: name, Type: t, Category: c, TypName: tn}) // VARIADIC: called with an array
			}
			if mode == "o" || mode == "b" || mode == "t" {
				out = append(out, &Column{Name: name, Type: t, Category: c, TypName: tn, Nullable: true})
			}
		}
		for i := len(f.Args) - int(ndefaults); i >= 0 && i < len(f.Args); i++ {
			f.Args[i].HasDefault = true
		}
		switch {
		case len(out) > 0 && (f.RetSet || len(out) > 1):
			f.Columns = out
		case relid != 0:
			if t := byOID[relid]; t != nil {
				f.Columns = t.Columns
			}
		}
		key := f.Schema + "." + f.Name
		cat.Functions[key] = append(cat.Functions[key], &f)
	}
	return rows.Err()
}
