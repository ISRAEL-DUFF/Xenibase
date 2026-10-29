// Package pgverify compares two databases the way imports and promotions
// check a copy: per-table row counts and sequence values.
package pgverify

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// TableCount is one table's row count.
type TableCount struct {
	Table string
	Rows  int64
}

// TableCounts counts the rows of every user table on conn in schemas (nil:
// every schema), by name. Only those tables are read: a source login may
// read the schemas it exports and nothing else.
func TableCounts(ctx context.Context, conn *pgx.Conn, schemas []string) ([]TableCount, error) {
	rows, err := conn.Query(ctx, `
		SELECT n.nspname, c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		  AND ($1::text[] IS NULL OR n.nspname = ANY($1))
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		ORDER BY 1, 2`, schemas)
	if err != nil {
		return nil, err
	}
	var names [][2]string
	for rows.Next() {
		var n [2]string
		if err := rows.Scan(&n[0], &n[1]); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TableCount, 0, len(names))
	for _, n := range names {
		var c int64
		qn := ident(n[0]) + "." + ident(n[1])
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+qn).Scan(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", qn, err)
		}
		out = append(out, TableCount{Table: n[0] + "." + n[1], Rows: c})
	}
	return out, nil
}

// SequenceValue is one sequence's state.
type SequenceValue struct {
	Name   string
	Value  *int64
	Called bool
}

// SequenceValues reads every user sequence's last value.
func SequenceValues(ctx context.Context, conn *pgx.Conn, schemas []string) ([]SequenceValue, error) {
	rows, err := conn.Query(ctx, `
		SELECT schemaname || '.' || sequencename, last_value FROM pg_sequences
		WHERE schemaname = ANY($1) ORDER BY 1`, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SequenceValue
	for rows.Next() {
		var v SequenceValue
		if err := rows.Scan(&v.Name, &v.Value); err != nil {
			return nil, err
		}
		v.Called = v.Value != nil
		out = append(out, v)
	}
	return out, rows.Err()
}

// UserSchemas lists the schemas on conn that hold user objects.
func UserSchemas(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT nspname FROM pg_namespace n
		WHERE nspname NOT IN ('pg_catalog', 'information_schema') AND nspname NOT LIKE 'pg\_%'
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = n.oid AND d.deptype = 'e')
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Result is a comparison of a copy with its source.
type Result struct {
	Tables    int
	Rows      int64
	Sequences int
	// Differences, one line each; empty means the copy matches.
	Missing, Counts, Sequence []string
}

// OK reports whether the copy matches.
func (r Result) OK() bool { return len(r.Missing)+len(r.Counts)+len(r.Sequence) == 0 }

// Summary describes a mismatch in one line.
func (r Result) Summary() string {
	var parts []string
	if len(r.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing tables: %s", strings.Join(r.Missing, ", ")))
	}
	if len(r.Counts) > 0 {
		parts = append(parts, fmt.Sprintf("row counts differ: %s", strings.Join(r.Counts, ", ")))
	}
	if len(r.Sequence) > 0 {
		parts = append(parts, fmt.Sprintf("sequences differ: %s", strings.Join(r.Sequence, ", ")))
	}
	return strings.Join(parts, "; ")
}

// Compare checks dst against src for the tables and sequences in schemas
// (nil: every user schema of src).
func Compare(ctx context.Context, src, dst *pgx.Conn, schemas []string) (Result, error) {
	var r Result
	if schemas == nil {
		var err error
		if schemas, err = UserSchemas(ctx, src); err != nil {
			return r, err
		}
	}
	in := map[string]bool{}
	for _, s := range schemas {
		in[s] = true
	}
	sc, err := TableCounts(ctx, src, schemas)
	if err != nil {
		return r, fmt.Errorf("source: %w", err)
	}
	dc, err := TableCounts(ctx, dst, schemas)
	if err != nil {
		return r, fmt.Errorf("target: %w", err)
	}
	got := map[string]int64{}
	for _, c := range dc {
		got[c.Table] = c.Rows
	}
	for _, c := range sc {
		schema, _, _ := strings.Cut(c.Table, ".")
		if !in[schema] {
			continue
		}
		r.Tables++
		r.Rows += c.Rows
		n, ok := got[c.Table]
		switch {
		case !ok:
			r.Missing = append(r.Missing, c.Table)
		case n != c.Rows:
			r.Counts = append(r.Counts, fmt.Sprintf("%s (%d vs %d)", c.Table, c.Rows, n))
		}
	}
	ss, err := SequenceValues(ctx, src, schemas)
	if err != nil {
		return r, fmt.Errorf("source sequences: %w", err)
	}
	ds, err := SequenceValues(ctx, dst, schemas)
	if err != nil {
		return r, fmt.Errorf("target sequences: %w", err)
	}
	dv := map[string]SequenceValue{}
	for _, v := range ds {
		dv[v.Name] = v
	}
	r.Sequences = len(ss)
	for _, v := range ss {
		d, ok := dv[v.Name]
		if !ok || (v.Value == nil) != (d.Value == nil) || (v.Value != nil && *v.Value != *d.Value) {
			r.Sequence = append(r.Sequence, v.Name)
		}
	}
	sort.Strings(r.Missing)
	return r, nil
}
