package insights

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/schemaedit"
	"github.com/israel-duff/xenibase/internal/store"
)

// Thresholds for the table heuristics (V3 §8).
const (
	bigTableRows     = 10_000 // a table worth an index
	seqTuplesPerScan = 1_000  // sequential scans that read this much each are expensive
	planQueries      = 10     // top queries whose plans are read for filter columns
	maxIndexColumns  = 3
)

// IndexReport is the Indexes tab: suggestions, unused and duplicate
// indexes, and whether hypopg can estimate.
type IndexReport struct {
	Suggestions []Suggestion
	Unused      []IndexInfo
	Duplicates  []Duplicate
	// HeavySeqScans are large tables read whole often, with no filter
	// column to suggest an index on.
	HeavySeqScans []TableScans
	// Hypopg: "installed" (estimates shown), "available" (enable it on the
	// Extensions page for estimates), or "unavailable".
	Hypopg string
	// StatsSince is when the index usage counters started.
	StatsSince *time.Time
}

// Suggestion is an index Xenibase suggests.
type Suggestion struct {
	Schema, Table string
	Columns       []string
	// Reasons: seq_scan_filter (top queries filter on these columns with a
	// sequential scan), unindexed_foreign_key.
	Reasons []string
	// QueryIDs are the top queries it would help.
	QueryIDs  []int64
	Statement string
	// Change is the schema change for the table editor's apply and "Save
	// as migration" (V2 §4.3).
	Change schemaedit.Change
	// Estimate is hypopg's, when installed.
	Estimate *Estimate
	// TableRows and SeqScans describe the table.
	TableRows float64
	SeqScans  int64
}

// Estimate is the planner's cost of the affected queries without and with
// the hypothetical index.
type Estimate struct {
	CostBefore, CostAfter float64
	// Improvement is 1 - after/before.
	Improvement float64
	// UsesIndex: the planner chose the hypothetical index.
	UsesIndex bool
}

// IndexInfo is an index with its size and use.
type IndexInfo struct {
	Schema, Table, Name string
	Definition          string
	Bytes               int64
	Scans               int64
}

// Duplicate is an index another one makes unnecessary.
type Duplicate struct {
	IndexInfo
	// Of is the index that covers it; Exact when they are identical, else
	// its columns are a leading prefix of Of's.
	Of    string
	Exact bool
}

// TableScans is a table's sequential scan statistics.
type TableScans struct {
	Schema, Table string
	Rows          float64
	SeqScans      int64
	SeqTupRead    int64
	IdxScans      int64
}

type existingIndex struct {
	schema, table, name, def, method string
	cols                             []string
	unique, primary, valid           bool
	bytes, scans                     int64
	key                              string // columns, opclasses, expressions, predicate
	constraint                       bool
	oid                              int64
}

// Indexes builds the Indexes tab for p: catalog reads as the project's
// role, plans of the top queries, and hypopg estimates.
func (s *Service) Indexes(ctx context.Context, p store.Project) (IndexReport, error) {
	out := IndexReport{Suggestions: []Suggestion{}, Unused: []IndexInfo{}, Duplicates: []Duplicate{}, HeavySeqScans: []TableScans{}, Hypopg: "unavailable"}
	if ok, err := s.Available(ctx, p); err != nil || !ok {
		return out, errors.Join(err, notAvailable(ok))
	}
	top, err := store.New(s.db).TopQueries(ctx, store.TopQueriesParams{ProjectID: p.ID, Since: s.Now().Add(-24 * time.Hour), Sort: "total", Lim: planQueries})
	if err != nil {
		return out, err
	}
	examples := map[int64]store.QueryText{}
	for _, t := range top {
		if qt, err := store.New(s.db).GetQueryText(ctx, store.GetQueryTextParams{ProjectID: p.ID, Queryid: t.Queryid}); err == nil {
			examples[t.Queryid] = qt
		}
	}
	// pg_stat_database is the admin's alone on a shared cluster.
	if since, err := s.statsSince(ctx, p); err == nil {
		out.StatsSince = since
	} else {
		s.log.Warn("insights: index statistics reset time", "project_id", p.ID, "err", err)
	}
	err = s.console.ReadOnly(ctx, p.ID, func(conn *pgx.Conn) error {
		idx, err := listIndexes(ctx, conn)
		if err != nil {
			return err
		}
		tables, err := tableScans(ctx, conn)
		if err != nil {
			return err
		}
		out.Hypopg, err = hypopgState(ctx, conn)
		if err != nil {
			return err
		}

		// Unused (never scanned, not backing a constraint) and duplicate indexes.
		for _, i := range idx {
			if i.scans == 0 && !i.unique && !i.primary && !i.constraint && i.valid {
				out.Unused = append(out.Unused, i.info())
			}
		}
		out.Duplicates = duplicates(idx)

		// Candidates: filter columns of sequential scans in the top queries...
		type cand struct {
			s       Suggestion
			queries []candQuery
		}
		cands := map[string]*cand{}
		add := func(schema, table string, cols []string, reason string, q *candQuery) {
			if len(cols) > maxIndexColumns {
				cols = cols[:maxIndexColumns]
			}
			if covered(idx, schema, table, cols) {
				return
			}
			k := schema + "." + table + "(" + strings.Join(cols, ",") + ")"
			c := cands[k]
			if c == nil {
				c = &cand{s: Suggestion{Schema: schema, Table: table, Columns: cols, Reasons: []string{}, QueryIDs: []int64{}}}
				cands[k] = c
			}
			if !slices.Contains(c.s.Reasons, reason) {
				c.s.Reasons = append(c.s.Reasons, reason)
			}
			if q != nil && !slices.Contains(c.s.QueryIDs, q.id) {
				c.s.QueryIDs = append(c.s.QueryIDs, q.id)
				c.queries = append(c.queries, *q)
			}
		}
		for _, t := range top {
			qt, ok := examples[t.Queryid]
			if !ok {
				continue
			}
			cq := candQuery{id: t.Queryid, stmt: qt.Query, generic: true}
			if qt.Example != nil {
				cq.stmt, cq.generic = *qt.Example, false
			}
			plan, err := savepointExplain(ctx, conn, cq.stmt, cq.generic)
			if err != nil {
				continue // not explainable (a utility statement, a dropped table)
			}
			for rel, cols := range plan.filters {
				schema, table := splitRel(rel)
				if tables[rel].Rows < bigTableRows || len(cols) == 0 {
					continue
				}
				add(schema, table, cols, "seq_scan_filter", &cq)
			}
		}
		// ...and foreign keys without an index on their columns.
		fks, err := unindexedFKs(ctx, conn)
		if err != nil {
			return err
		}
		for _, fk := range fks {
			add(fk.schema, fk.table, fk.cols, "unindexed_foreign_key", nil)
		}

		for _, c := range cands {
			sg := c.s
			t := tables[sg.Schema+"."+sg.Table]
			sg.TableRows, sg.SeqScans = t.Rows, t.SeqScans
			name := indexName(sg.Table, sg.Columns)
			yes := true
			sg.Change = schemaedit.Change{Kind: "create_index", Schema: sg.Schema, Table: sg.Table, Name: name, KeyColumns: sg.Columns, Concurrent: &yes}
			sg.Statement = fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s.%s (%s);", ident(name), ident(sg.Schema), ident(sg.Table), identList(sg.Columns))
			if out.Hypopg == "installed" && len(c.queries) > 0 {
				if e, err := estimate(ctx, conn, sg, c.queries); err == nil {
					sg.Estimate = &e
				} else {
					s.log.Debug("hypopg estimate", "project_id", p.ID, "err", err)
				}
			}
			out.Suggestions = append(out.Suggestions, sg)
		}
		sort.Slice(out.Suggestions, func(i, j int) bool {
			a, b := out.Suggestions[i], out.Suggestions[j]
			if len(a.QueryIDs) != len(b.QueryIDs) {
				return len(a.QueryIDs) > len(b.QueryIDs)
			}
			return a.Schema+a.Table < b.Schema+b.Table
		})

		// Large tables read whole that no suggestion covers.
		for rel, t := range tables {
			if t.Rows < bigTableRows || t.SeqScans == 0 || t.SeqTupRead/t.SeqScans < seqTuplesPerScan || t.SeqScans <= t.IdxScans {
				continue
			}
			if slices.ContainsFunc(out.Suggestions, func(sg Suggestion) bool { return sg.Schema+"."+sg.Table == rel }) {
				continue
			}
			out.HeavySeqScans = append(out.HeavySeqScans, t)
		}
		sort.Slice(out.HeavySeqScans, func(i, j int) bool { return out.HeavySeqScans[i].SeqTupRead > out.HeavySeqScans[j].SeqTupRead })
		return nil
	})
	return out, err
}

type candQuery struct {
	id      int64
	stmt    string
	generic bool
}

func (i existingIndex) info() IndexInfo {
	return IndexInfo{Schema: i.schema, Table: i.table, Name: i.name, Definition: i.def, Bytes: i.bytes, Scans: i.scans}
}

func listIndexes(ctx context.Context, conn *pgx.Conn) ([]existingIndex, error) {
	rows, err := conn.Query(ctx, `
		SELECT n.nspname, t.relname, c.relname, pg_get_indexdef(i.indexrelid), am.amname,
		  ARRAY(SELECT COALESCE(a.attname, '') FROM unnest(i.indkey) WITH ORDINALITY k(attnum, ord)
		        LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum ORDER BY k.ord)::text[],
		  i.indisunique, i.indisprimary, i.indisvalid, pg_relation_size(i.indexrelid), COALESCE(st.idx_scan, 0),
		  i.indkey::text || '|' || i.indclass::text || '|' || COALESCE(pg_get_expr(i.indexprs, i.indrelid), '') || '|' ||
		    COALESCE(pg_get_expr(i.indpred, i.indrelid), '') || '|' || am.amname,
		  EXISTS (SELECT 1 FROM pg_constraint co WHERE co.conindid = i.indexrelid), i.indexrelid::bigint
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_am am ON am.oid = c.relam
		LEFT JOIN pg_stat_user_indexes st ON st.indexrelid = i.indexrelid
		WHERE `+userSchemas+` ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, err
	}
	var out []existingIndex
	return out, forEach(rows, func(r pgx.Rows) error {
		var x existingIndex
		if err := r.Scan(&x.schema, &x.table, &x.name, &x.def, &x.method, &x.cols, &x.unique, &x.primary, &x.valid, &x.bytes, &x.scans,
			&x.key, &x.constraint, &x.oid); err != nil {
			return err
		}
		out = append(out, x)
		return nil
	})
}

// userSchemas excludes system schemas (as the console does).
const userSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'`

func tableScans(ctx context.Context, conn *pgx.Conn) (map[string]TableScans, error) {
	rows, err := conn.Query(ctx, `SELECT s.schemaname, s.relname, c.reltuples, s.seq_scan, s.seq_tup_read, COALESCE(s.idx_scan, 0)
		FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid`)
	if err != nil {
		return nil, err
	}
	out := map[string]TableScans{}
	return out, forEach(rows, func(r pgx.Rows) error {
		var t TableScans
		var seq, read *int64
		if err := r.Scan(&t.Schema, &t.Table, &t.Rows, &seq, &read, &t.IdxScans); err != nil {
			return err
		}
		if seq != nil {
			t.SeqScans = *seq
		}
		if read != nil {
			t.SeqTupRead = *read
		}
		out[t.Schema+"."+t.Table] = t
		return nil
	})
}

func hypopgState(ctx context.Context, conn *pgx.Conn) (string, error) {
	var installed, available bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'hypopg'),
		EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'hypopg')`).Scan(&installed, &available); err != nil {
		return "", err
	}
	switch {
	case installed:
		return "installed", nil
	case available:
		return "available", nil
	}
	return "unavailable", nil
}

// covered: an existing valid btree index leads with cols.
func covered(idx []existingIndex, schema, table string, cols []string) bool {
	for _, i := range idx {
		if i.schema != schema || i.table != table || !i.valid || i.method != "btree" || len(i.cols) < len(cols) {
			continue
		}
		if slices.Equal(i.cols[:len(cols)], cols) {
			return true
		}
	}
	return false
}

// duplicates: identical indexes, and plain btree indexes whose columns
// lead another btree index on the same table.
func duplicates(idx []existingIndex) []Duplicate {
	out := []Duplicate{}
	for a, x := range idx {
		for b, y := range idx {
			if a == b || x.schema != y.schema || x.table != y.table {
				continue
			}
			switch {
			case x.key == y.key:
				// Report the newer of an identical pair, keeping a constraint's.
				if !x.constraint && (y.constraint || x.oid > y.oid) {
					out = append(out, Duplicate{IndexInfo: x.info(), Of: y.name, Exact: true})
				}
			case !x.unique && !x.constraint && x.method == "btree" && y.method == "btree" && len(x.cols) < len(y.cols) &&
				!slices.Contains(x.cols, "") && slices.Equal(x.cols, y.cols[:len(x.cols)]) && !strings.Contains(x.def, " WHERE "):
				out = append(out, Duplicate{IndexInfo: x.info(), Of: y.name})
			default:
				continue
			}
			break
		}
	}
	return out
}

type fkCols struct {
	schema, table string
	cols          []string
}

func unindexedFKs(ctx context.Context, conn *pgx.Conn) ([]fkCols, error) {
	rows, err := conn.Query(ctx, `
		SELECT n.nspname, t.relname,
		  ARRAY(SELECT a.attname FROM unnest(co.conkey) WITH ORDINALITY k(attnum, ord)
		        JOIN pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.attnum ORDER BY k.ord)::text[]
		FROM pg_constraint co
		JOIN pg_class t ON t.oid = co.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE co.contype = 'f' AND `+userSchemas)
	if err != nil {
		return nil, err
	}
	var out []fkCols
	return out, forEach(rows, func(r pgx.Rows) error {
		var f fkCols
		if err := r.Scan(&f.schema, &f.table, &f.cols); err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
}

func savepointExplain(ctx context.Context, conn *pgx.Conn, stmt string, generic bool) (planResult, error) {
	if _, err := conn.Exec(ctx, "SAVEPOINT xenibase_explain"); err != nil {
		return planResult{}, err
	}
	plan, err := explain(ctx, conn, stmt, generic, true)
	if err != nil {
		_, _ = conn.Exec(ctx, "ROLLBACK TO SAVEPOINT xenibase_explain")
		return plan, err
	}
	_, err = conn.Exec(ctx, "RELEASE SAVEPOINT xenibase_explain")
	return plan, err
}

// estimate plans the suggestion's queries without and with a hypothetical
// index (hypopg), then removes it.
func estimate(ctx context.Context, conn *pgx.Conn, sg Suggestion, qs []candQuery) (Estimate, error) {
	var e Estimate
	var before []float64
	for _, q := range qs {
		p, err := savepointExplain(ctx, conn, q.stmt, q.generic)
		if err != nil {
			return e, err
		}
		before = append(before, p.cost)
	}
	ddl := fmt.Sprintf("CREATE INDEX ON %s.%s (%s)", provision.Ident(sg.Schema), provision.Ident(sg.Table), identList(sg.Columns))
	var hypoName string
	if err := conn.QueryRow(ctx, `SELECT indexname FROM hypopg_create_index($1)`, ddl).Scan(&hypoName); err != nil {
		return e, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT hypopg_reset()`) }()
	for i, q := range qs {
		p, err := savepointExplain(ctx, conn, q.stmt, q.generic)
		if err != nil {
			return e, err
		}
		e.CostBefore += before[i]
		e.CostAfter += p.cost
		e.UsesIndex = e.UsesIndex || slices.ContainsFunc(p.indexes, func(n string) bool { return strings.Contains(n, hypoName) || strings.HasPrefix(n, "<") })
	}
	if e.CostBefore > 0 {
		e.Improvement = 1 - e.CostAfter/e.CostBefore
	}
	return e, nil
}

func splitRel(rel string) (string, string) {
	if i := strings.IndexByte(rel, '.'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "public", rel
}

func identList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = ident(c)
	}
	return strings.Join(q, ", ")
}

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// reserved are the keywords most likely to be column names.
var reserved = []string{"all", "and", "any", "array", "as", "asc", "case", "check", "collate", "column", "constraint", "create",
	"default", "desc", "distinct", "do", "else", "end", "for", "foreign", "from", "grant", "group", "having", "in", "into", "limit",
	"not", "null", "offset", "on", "only", "or", "order", "primary", "references", "select", "table", "then", "to", "union",
	"unique", "user", "using", "when", "where", "window", "with"}

// ident quotes a name only when it must be.
func ident(s string) string {
	if plainIdent.MatchString(s) && !slices.Contains(reserved, s) {
		return s
	}
	return provision.Ident(s)
}

// indexName is <table>_<cols>_idx, within Postgres' 63 bytes.
func indexName(table string, cols []string) string {
	n := table + "_" + strings.Join(cols, "_") + "_idx"
	if len(n) > 63 {
		n = n[:59] + "_idx"
	}
	return n
}

// statsSince is when p's database's statistics counters started: their
// last reset, else the server's start. It reads pg_stat_database as the
// instance admin, since tenants on a shared cluster may not.
func (s *Service) statsSince(ctx context.Context, p store.Project) (*time.Time, error) {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	var since *time.Time
	err = conn.QueryRow(ctx, `SELECT coalesce(stats_reset, pg_postmaster_start_time()) FROM pg_stat_database WHERE datname = current_database()`).Scan(&since)
	return since, err
}
