package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The data API's writes (V4 §3.3) and functions (§3.4). Every write runs
// as the caller's role in the request's one transaction, so row-level
// security's USING and WITH CHECK apply, and a batch is all or nothing.

const (
	maxWriteRows       = 1000
	defaultMaxAffected = 1000
	maxMaxAffected     = 100000
	maxBatchOps        = 50
)

// writeGate is stricter than the read gate: public tables are readable
// without row-level security, never writable without it.
func (p *project) writeGate(cat *Catalog, role string) gate {
	if role == "service" {
		return nil
	}
	return func(t *Table) *apiError {
		switch t.Kind {
		case kindView:
			if t.Invoker {
				return p.viewGate(cat, role, t, true)
			}
		case kindTable, kindPart, kindForeign:
			if t.RLS {
				return nil
			}
		}
		return &apiError{Status: http.StatusForbidden, Code: "rls_required", Details: map[string]any{"table": t.Name},
			Message: fmt.Sprintf("%s has no row-level security, so it can't be written with the publishable key: enable it and add policies", t.Name)}
	}
}

// writeOpts are a write's options.
type writeOpts struct {
	Select      []SelectItem
	Minimal     bool
	OnConflict  []string
	Ignore      bool // resolution=ignore
	MaxAffected int
}

// writeResult is one write's outcome.
type writeResult struct {
	Rows     []json.RawMessage
	Affected int
}

func (r writeResult) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"affected":` + strconv.Itoa(r.Affected))
	if r.Rows != nil {
		b.WriteString(`,"data":[`)
		for i, row := range r.Rows {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(row)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// decodeRows reads an object or an array of objects.
func decodeRows(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, badRequest("invalid_body", "send a row or a list of rows")
	}
	var rows []map[string]json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, badRequest("invalid_body", "rows are JSON objects")
		}
	} else {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, badRequest("invalid_body", "a row is a JSON object")
		}
		rows = []map[string]json.RawMessage{one}
	}
	if len(rows) == 0 || len(rows) > maxWriteRows {
		return nil, badRequest("invalid_body", "send 1 to %d rows", maxWriteRows)
	}
	return rows, nil
}

// columnsOf is the union of rows' keys, checked against t.
func columnsOf(t *Table, rows []map[string]json.RawMessage) ([]string, error) {
	seen := map[string]bool{}
	var cols []string
	for _, r := range rows {
		for k := range r {
			if seen[k] {
				continue
			}
			if t.Col(k) == nil {
				return nil, &apiError{Status: 400, Code: "unknown_column", Message: fmt.Sprintf("%s has no column %q", t.Name, k)}
			}
			seen[k] = true
			cols = append(cols, k)
		}
	}
	sort.Strings(cols)
	return cols, nil
}

func quoteList(alias string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		if alias != "" {
			out[i] = alias + "." + qi(c)
		} else {
			out[i] = qi(c)
		}
	}
	return strings.Join(out, ", ")
}

// returning wraps a data-modifying CTE: its rows as the select list's
// JSON (or none with Minimal).
func (b *builder) returning(t *Table, cte string, opts writeOpts) (string, error) {
	if opts.Minimal {
		return "WITH w AS (" + cte + ") SELECT NULL::json FROM w", nil
	}
	sel := opts.Select
	if len(sel) == 0 {
		sel = []SelectItem{{Star: true}}
	}
	alias := b.nextAlias()
	cols, err := b.selectList(t, alias, sel, 0)
	if err != nil {
		return "", err
	}
	return "WITH w AS (" + cte + ") SELECT (SELECT row_to_json(r) FROM (SELECT " + strings.Join(cols, ", ") + ") r) FROM w " + alias, nil
}

func runWrite(ctx context.Context, tx pgx.Tx, sql string, args []any, opts writeOpts) (writeResult, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return writeResult{}, err
	}
	defer rows.Close()
	res := writeResult{}
	if !opts.Minimal {
		res.Rows = []json.RawMessage{}
	}
	size := 0
	for rows.Next() {
		res.Affected++
		if opts.Minimal {
			continue
		}
		v := rows.RawValues()[0]
		size += len(v)
		if size > maxResultBytes {
			return writeResult{}, &apiError{Status: http.StatusRequestEntityTooLarge, Code: "result_too_large",
				Message: "the returned rows are over 10 MB: use return=minimal or select fewer columns"}
		}
		res.Rows = append(res.Rows, append(json.RawMessage(nil), v...))
	}
	return res, rows.Err()
}

// insert inserts rows (an upsert with OnConflict).
func (b *builder) insert(ctx context.Context, tx pgx.Tx, t *Table, raw json.RawMessage, opts writeOpts) (writeResult, error) {
	if t.Kind == kindMatView {
		return writeResult{}, badRequest("not_writable", "%s is a materialized view", t.Name)
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return writeResult{}, err
	}
	cols, err := columnsOf(t, rows)
	if err != nil {
		return writeResult{}, err
	}
	var cte string
	if len(cols) == 0 {
		if len(rows) != 1 {
			return writeResult{}, badRequest("invalid_body", "rows without columns insert one at a time")
		}
		cte = "INSERT INTO " + t.Qualified() + " DEFAULT VALUES RETURNING *"
	} else {
		body, _ := json.Marshal(rows)
		cte = "INSERT INTO " + t.Qualified() + " (" + quoteList("", cols) + ") SELECT " + quoteList("", cols) +
			" FROM json_populate_recordset(NULL::" + t.Qualified() + ", " + b.param(string(body)) + "::json)"
	}
	if len(opts.OnConflict) > 0 {
		for _, c := range opts.OnConflict {
			if t.Col(c) == nil {
				return writeResult{}, &apiError{Status: 400, Code: "unknown_column", Message: fmt.Sprintf("%s has no column %q", t.Name, c)}
			}
		}
		cte = strings.TrimSuffix(cte, " RETURNING *")
		cte += " ON CONFLICT (" + quoteList("", opts.OnConflict) + ")"
		var set []string
		conflict := map[string]bool{}
		for _, c := range opts.OnConflict {
			conflict[c] = true
		}
		for _, c := range cols {
			if !conflict[c] {
				set = append(set, qi(c)+" = EXCLUDED."+qi(c))
			}
		}
		if opts.Ignore || len(set) == 0 {
			cte += " DO NOTHING"
		} else {
			cte += " DO UPDATE SET " + strings.Join(set, ", ")
		}
	}
	if !strings.HasSuffix(cte, "RETURNING *") {
		cte += " RETURNING *"
	}
	sql, err := b.returning(t, cte, opts)
	if err != nil {
		return writeResult{}, err
	}
	return runWrite(ctx, tx, sql, b.args, opts)
}

// target is the rows an update or delete reaches: one by key, or by a
// filter, which is required.
func (b *builder) target(t *Table, alias string, pk *string, where Filter) (string, error) {
	if pk != nil {
		if len(t.PK) != 1 {
			return "", badRequest("no_single_key", "%s has no single-column primary key", t.Name)
		}
		c := t.Col(t.PK[0])
		return alias + "." + qi(c.Name) + " = " + b.param(*pk) + "::" + c.Type, nil
	}
	if countFilters(where) == 0 {
		return "", badRequest("filter_required", "an update or delete needs a filter (where=…) or a key; unfiltered writes are refused")
	}
	if countFilters(where) > maxFilters {
		return "", badRequest("too_many_filters", "at most %d filters", maxFilters)
	}
	return b.where(t, alias, where)
}

func checkAffected(res writeResult, opts writeOpts) error {
	limit := opts.MaxAffected
	if limit == 0 {
		limit = defaultMaxAffected
	}
	if res.Affected > limit {
		return &apiError{Status: http.StatusBadRequest, Code: "too_many_rows",
			Message: fmt.Sprintf("the write would change %d rows, more than max_affected (%d); nothing was changed", res.Affected, limit),
			Details: map[string]any{"affected": res.Affected, "max_affected": limit}}
	}
	return nil
}

func (b *builder) update(ctx context.Context, tx pgx.Tx, t *Table, pk *string, where Filter, raw json.RawMessage, opts writeOpts) (writeResult, error) {
	if t.Kind == kindMatView {
		return writeResult{}, badRequest("not_writable", "%s is a materialized view", t.Name)
	}
	var set map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &set); err != nil || len(set) == 0 {
		return writeResult{}, badRequest("invalid_body", "send the columns to change as a JSON object")
	}
	cols, err := columnsOf(t, []map[string]json.RawMessage{set})
	if err != nil {
		return writeResult{}, err
	}
	body, _ := json.Marshal(set)
	alias := b.nextAlias()
	values := b.param(string(body))
	cond, err := b.target(t, alias, pk, where)
	if err != nil {
		return writeResult{}, err
	}
	cte := "UPDATE " + t.Qualified() + " AS " + alias + " SET (" + quoteList("", cols) + ") = (SELECT " + quoteList("", cols) +
		" FROM json_populate_record(NULL::" + t.Qualified() + ", " + values + "::json)) WHERE " + cond + " RETURNING " + alias + ".*"
	sql, err := b.returning(t, cte, opts)
	if err != nil {
		return writeResult{}, err
	}
	res, err := runWrite(ctx, tx, sql, b.args, opts)
	if err != nil {
		return res, err
	}
	return res, checkAffected(res, opts)
}

func (b *builder) delete(ctx context.Context, tx pgx.Tx, t *Table, pk *string, where Filter, opts writeOpts) (writeResult, error) {
	if t.Kind == kindMatView {
		return writeResult{}, badRequest("not_writable", "%s is a materialized view", t.Name)
	}
	alias := b.nextAlias()
	cond, err := b.target(t, alias, pk, where)
	if err != nil {
		return writeResult{}, err
	}
	cte := "DELETE FROM " + t.Qualified() + " AS " + alias + " WHERE " + cond + " RETURNING " + alias + ".*"
	sql, err := b.returning(t, cte, opts)
	if err != nil {
		return writeResult{}, err
	}
	res, err := runWrite(ctx, tx, sql, b.args, opts)
	if err != nil {
		return res, err
	}
	return res, checkAffected(res, opts)
}

// writeOptsFromURL reads select, return, on_conflict, resolution and
// max_affected.
func writeOptsFromURL(v url.Values, allowed map[string]bool) (writeOpts, Filter, error) {
	var o writeOpts
	var where Filter
	for k := range v {
		if !allowed[k] {
			return o, where, badRequest("unknown_parameter", "unknown parameter %q", k)
		}
	}
	var err error
	if s := v.Get("select"); s != "" {
		if o.Select, err = ParseSelect(s, 0); err != nil {
			return o, where, err
		}
	}
	switch v.Get("return") {
	case "", "representation":
	case "minimal":
		o.Minimal = true
	default:
		return o, where, badRequest("invalid_return", "return is minimal or representation")
	}
	if s := v.Get("on_conflict"); s != "" {
		for _, c := range strings.Split(s, ",") {
			o.OnConflict = append(o.OnConflict, strings.TrimSpace(c))
		}
	}
	switch v.Get("resolution") {
	case "", "merge":
	case "ignore":
		o.Ignore = true
	default:
		return o, where, badRequest("invalid_resolution", "resolution is merge or ignore")
	}
	if s := v.Get("max_affected"); s != "" {
		if o.MaxAffected, err = strconv.Atoi(s); err != nil || o.MaxAffected < 1 || o.MaxAffected > maxMaxAffected {
			return o, where, badRequest("invalid_max_affected", "max_affected is 1 to %d", maxMaxAffected)
		}
	}
	for _, w := range v["where"] {
		f, err := ParseCondition(w)
		if err != nil {
			return o, where, err
		}
		where.And = append(where.And, f)
	}
	for _, s := range v["or"] {
		f, err := ParseOr(s)
		if err != nil {
			return o, where, err
		}
		where.And = append(where.And, f)
	}
	return o, where, nil
}

var (
	insertParams = map[string]bool{"select": true, "return": true, "on_conflict": true, "resolution": true, "apikey": true}
	changeParams = map[string]bool{"select": true, "return": true, "where": true, "or": true, "max_affected": true, "apikey": true}
)

func readBody(c *call) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(c.r.Body, maxBodyBytes*8+1))
	if err != nil || len(body) > maxBodyBytes*8 {
		c.apiFail(&apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "a write is at most 8 MB"})
		return nil, false
	}
	return body, true
}

// inTx runs fn in the request's transaction with the catalog; an
// *apiError from fn rolls back and is the response. With an
// Idempotency-Key the key is claimed first and the answer kept beside the
// write, so a repeat gets the first answer.
func (e *Edge) inTx(c *call, req Request, key *idempotency, fn func(ctx context.Context, tx pgx.Tx, cat *Catalog) (any, int, error)) {
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+3*time.Second)
	defer cancel()
	var out []byte
	status := http.StatusOK
	err := e.WithRequest(ctx, c.p, req, func(tx pgx.Tx) error {
		if err := key.claim(ctx, tx); err != nil {
			return err
		}
		cat, _, err := e.catalog(ctx, c.p, tx)
		if err != nil {
			return err
		}
		var v any
		if v, status, err = fn(ctx, tx, cat); err != nil {
			return err
		}
		if out, err = json.Marshal(v); err != nil {
			return err
		}
		out = append(out, '\n')
		return key.store(ctx, tx, status, out)
	})
	if err != nil {
		var rp *replayed
		if errors.As(err, &rp) {
			c.writeReplay(rp)
			return
		}
		var ae *apiError
		if errors.As(err, &ae) {
			c.apiFail(ae)
			return
		}
		e.dataDBError(c, err)
		return
	}
	// Committed: drop the cached reads it could have changed (V4.1 §10)
	// before answering, so the caller's next read sees its write.
	e.dropWritten(c, c.wrote, false)
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(out)
}

func (e *Edge) findWritable(cat *Catalog, p *project, role, name string) (*Table, error) {
	t := cat.Find(name)
	if t == nil {
		return nil, &apiError{Status: http.StatusNotFound, Code: "unknown_table",
			Message: fmt.Sprintf("no table or view %q in the exposed schemas (%s)", name, strings.Join(cat.Schemas, ", "))}
	}
	if g := p.writeGate(cat, role); g != nil {
		if ge := g(t); ge != nil {
			return nil, ge
		}
	}
	return t, nil
}

// write handles POST /data/v1/{table}, PATCH and DELETE on a table or a row.
func (e *Edge) write(c *call, req Request, name string, pk *string) {
	m := c.r.Method
	allowed := changeParams
	if m == http.MethodPost {
		allowed = insertParams
	}
	opts, where, err := writeOptsFromURL(c.r.URL.Query(), allowed)
	if err != nil {
		c.apiFail(asAPIError(err))
		return
	}
	var body []byte
	if m != http.MethodDelete {
		var ok bool
		if body, ok = readBody(c); !ok {
			return
		}
	}
	key, ae := idempotencyOf(c, req, body)
	if ae != nil {
		c.apiFail(ae)
		return
	}
	e.inTx(c, req, key, func(ctx context.Context, tx pgx.Tx, cat *Catalog) (any, int, error) {
		t, err := e.findWritable(cat, c.p, req.Role, name)
		if err != nil {
			return nil, 0, err
		}
		c.wrote = append(c.wrote, t.Schema+"."+t.Name)
		b := &builder{cat: cat, gate: c.p.gateFor(cat, req.Role)}
		var res writeResult
		status := http.StatusOK
		switch m {
		case http.MethodPost:
			res, err = b.insert(ctx, tx, t, body, opts)
			status = http.StatusCreated
		case http.MethodPatch:
			res, err = b.update(ctx, tx, t, pk, where, body, opts)
		case http.MethodDelete:
			res, err = b.delete(ctx, tx, t, pk, where, opts)
		}
		if err != nil {
			return nil, 0, err
		}
		if pk != nil && res.Affected == 0 {
			return nil, 0, &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "no row with that key (or you can't change it)"}
		}
		return res, status, nil
	})
}

// batchOp is one operation of POST /data/v1/batch.
type batchOp struct {
	Op          string           `json:"op"` // insert | upsert | update | delete
	Table       string           `json:"table"`
	Rows        json.RawMessage  `json:"rows"`
	Set         json.RawMessage  `json:"set"`
	Where       json.RawMessage  `json:"where"`
	Key         *json.RawMessage `json:"key"`
	OnConflict  []string         `json:"on_conflict"`
	Resolution  string           `json:"resolution"`
	Return      string           `json:"return"`
	Select      string           `json:"select"`
	MaxAffected int              `json:"max_affected"`
}

// batch runs up to 50 writes in one transaction (V4 §3.3).
func (e *Edge) batch(c *call, req Request) {
	body, ok := readBody(c)
	if !ok {
		return
	}
	var in struct {
		Operations []batchOp `json:"operations"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		c.apiFail(badRequest("invalid_body", "a batch is {\"operations\":[…]}: %v", err))
		return
	}
	if len(in.Operations) == 0 || len(in.Operations) > maxBatchOps {
		c.apiFail(badRequest("invalid_body", "a batch has 1 to %d operations", maxBatchOps))
		return
	}
	key, ae := idempotencyOf(c, req, body)
	if ae != nil {
		c.apiFail(ae)
		return
	}
	e.inTx(c, req, key, func(ctx context.Context, tx pgx.Tx, cat *Catalog) (any, int, error) {
		results := make([]writeResult, 0, len(in.Operations))
		for i, op := range in.Operations {
			if t := cat.Find(op.Table); t != nil {
				c.wrote = append(c.wrote, t.Schema+"."+t.Name)
			}
			res, err := e.batchOne(ctx, tx, cat, c.p, req, op)
			if err != nil {
				var ae *apiError
				if !errors.As(err, &ae) {
					ae = pgAPIError(c, err)
				}
				if ae != nil {
					if ae.Details == nil {
						ae.Details = map[string]any{}
					}
					ae.Details["operation"] = i
					ae.Message = fmt.Sprintf("operation %d: %s (nothing was changed)", i, ae.Message)
					return nil, 0, ae
				}
				return nil, 0, err
			}
			results = append(results, res)
		}
		return map[string]any{"results": results}, http.StatusOK, nil
	})
}

func (e *Edge) batchOne(ctx context.Context, tx pgx.Tx, cat *Catalog, p *project, req Request, op batchOp) (writeResult, error) {
	t, err := e.findWritable(cat, p, req.Role, op.Table)
	if err != nil {
		return writeResult{}, err
	}
	opts := writeOpts{OnConflict: op.OnConflict, Ignore: op.Resolution == "ignore", Minimal: op.Return == "minimal", MaxAffected: op.MaxAffected}
	if op.Select != "" {
		if opts.Select, err = ParseSelect(op.Select, 0); err != nil {
			return writeResult{}, err
		}
	}
	var where Filter
	if len(op.Where) > 0 && string(op.Where) != "null" {
		if where, err = filterFromJSON(op.Where, 0); err != nil {
			return writeResult{}, err
		}
	}
	var pk *string
	if op.Key != nil {
		var s string
		if json.Unmarshal(*op.Key, &s) != nil {
			s = strings.TrimSpace(string(*op.Key))
		}
		pk = &s
	}
	b := &builder{cat: cat, gate: p.gateFor(cat, req.Role)}
	var res writeResult
	switch op.Op {
	case "insert":
		res, err = b.insert(ctx, tx, t, op.Rows, opts)
	case "upsert":
		if len(opts.OnConflict) == 0 {
			if len(t.PK) == 0 {
				return writeResult{}, badRequest("invalid_body", "an upsert into %s needs on_conflict", t.Name)
			}
			opts.OnConflict = t.PK
		}
		res, err = b.insert(ctx, tx, t, op.Rows, opts)
	case "update":
		res, err = b.update(ctx, tx, t, pk, where, op.Set, opts)
	case "delete":
		res, err = b.delete(ctx, tx, t, pk, where, opts)
	default:
		return writeResult{}, badRequest("invalid_body", "op is insert, upsert, update or delete, not %q", op.Op)
	}
	if err == nil && pk != nil && res.Affected == 0 {
		return res, &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "no row with that key (or you can't change it)"}
	}
	return res, err
}
