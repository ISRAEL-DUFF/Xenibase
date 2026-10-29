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
	// Unprotected are the relations a security_invoker view reads that
	// nothing protects ("schema.name": tables without row-level security,
	// materialized views, views that run as their owner), through the
	// invoker views it reads too; CallsFunctions says it, or one of those
	// views, calls a function of the project's.
	Unprotected    []string
	CallsFunctions bool
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
	// Reach is, by request role, the relations it holds privileges on that
	// nothing protects (ReachSQL); the edge fills it in.
	Reach map[string][]string
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

// userSchemas matches the project's own schemas, in a query with n as
// pg_namespace: not the system's or the platform's.
const userSchemas = `n.nspname NOT LIKE 'pg\_%' AND n.nspname NOT LIKE 'xb\_%' AND n.nspname NOT IN ('information_schema', 'xenibase')`

// FingerprintSQL changes when the relations, columns, constraints or
// functions of the schemas in $1 change, or the privileges and
// row-level security of any relation of the project's (a view or a
// function in $1 may read it).
const FingerprintSQL = `SELECT
  (SELECT coalesce(sum(c.xmin::text::bigint), 0) + count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = ANY($1) OR (` + userSchemas + `))::text || ':' ||
  (SELECT coalesce(sum(a.xmin::text::bigint), 0) + count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace WHERE (n.nspname = ANY($1) OR (` + userSchemas + `)) AND a.attnum > 0)::text || ':' ||
  (SELECT coalesce(sum(k.xmin::text::bigint), 0) + count(*) FROM pg_constraint k JOIN pg_namespace n ON n.oid = k.connamespace
    WHERE n.nspname = ANY($1))::text || ':' ||
  (SELECT coalesce(sum(f.xmin::text::bigint), 0) + count(*) FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace
    WHERE n.nspname = ANY($1))::text`

// unprotected is true for a relation c (with n its pg_namespace) that
// nothing protects: a table or foreign table without row-level security,
// a materialized view, or a view that runs as its owner. The project's own
// relations only, not an extension's.
const unprotected = `c.relkind IN ('r', 'p', 'f', 'v', 'm') AND ` + userSchemas + `
  AND NOT CASE c.relkind WHEN 'v' THEN coalesce(c.reloptions::text[] && ARRAY['security_invoker=true', 'security_invoker=on', 'security_invoker=1'], false)
    WHEN 'm' THEN false ELSE c.relrowsecurity END
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')`

// ReachSQL lists the relations role $1 holds privileges on that nothing
// protects, where it may read or change every row, as "schema.name"; the
// public tables in $2 ("schema.table", or "table" in public) may be read.
// A function that runs as the caller reaches whatever the caller may, so
// the data API calls none for a role this lists.
const ReachSQL = `SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE ` + unprotected + `
    AND (has_table_privilege($1, c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE') OR has_any_column_privilege($1, c.oid, 'INSERT, UPDATE')
      OR (has_any_column_privilege($1, c.oid, 'SELECT')
        AND NOT ((n.nspname || '.' || c.relname) = ANY ($2) OR (n.nspname = 'public' AND c.relname = ANY ($2)))))
  ORDER BY 1 LIMIT 20`

// Reach runs ReachSQL for role.
func Reach(ctx context.Context, tx Querier, role string, public []string) ([]string, error) {
	if public == nil {
		public = []string{}
	}
	rows, err := tx.Query(ctx, ReachSQL, role, public)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

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
	if err := introspectViews(ctx, tx, byOID); err != nil {
		return nil, err
	}
	return cat, introspectFunctions(ctx, tx, cat, byOID)
}

// introspectViews finds what each security_invoker view reads, following
// its rule's dependencies through the invoker views it reads: the
// relations nothing protects, and whether it calls a function of the
// project's (which reads as the caller).
func introspectViews(ctx context.Context, tx Querier, byOID map[uint32]*Table) error {
	var views []uint32
	for oid, t := range byOID {
		if t.Kind == KindView && t.Invoker {
			views = append(views, oid)
		}
	}
	if len(views) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE dep(view, cls, obj) AS (
		  SELECT r.ev_class, d.refclassid, d.refobjid FROM pg_rewrite r
		    JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = r.oid
		  WHERE r.ev_class = ANY($1) AND d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass) AND d.refobjid <> r.ev_class
		  UNION
		  SELECT dep.view, d.refclassid, d.refobjid FROM dep
		    JOIN pg_class c ON dep.cls = 'pg_class'::regclass AND c.oid = dep.obj AND c.relkind = 'v'
		      AND coalesce(c.reloptions::text[] && ARRAY['security_invoker=true', 'security_invoker=on', 'security_invoker=1'], false)
		    JOIN pg_rewrite r ON r.ev_class = c.oid
		    JOIN pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = r.oid
		  WHERE d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass) AND d.refobjid <> r.ev_class
		)
		SELECT dep.view, n.nspname || '.' || c.relname, false FROM dep JOIN pg_class c ON dep.cls = 'pg_class'::regclass AND c.oid = dep.obj
		  JOIN pg_namespace n ON n.oid = c.relnamespace WHERE `+unprotected+`
		UNION
		SELECT dep.view, '', true FROM dep JOIN pg_proc p ON dep.cls = 'pg_proc'::regclass AND p.oid = dep.obj
		  JOIN pg_namespace n ON n.oid = p.pronamespace WHERE `+userSchemas+`
		    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
		ORDER BY 1, 2`, views)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var rel string
		var fn bool
		if err := rows.Scan(&oid, &rel, &fn); err != nil {
			return err
		}
		t := byOID[oid]
		if fn {
			t.CallsFunctions = true
		} else {
			t.Unprotected = append(t.Unprotected, rel)
		}
	}
	return rows.Err()
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
