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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// The data API (V4 §3): reads in this milestone.

const (
	maxResultBytes = 10 << 20
	countTimeout   = 2 * time.Second
	maxBodyBytes   = 1 << 20
)

// gateFor is the RLS gate for the request's role (V4 §3.6): anon and user
// may read a table only with row-level security on (or marked public), and
// a view only when it runs as the invoker and what it reads passes too.
func (p *project) gateFor(cat *Catalog, role string) gate {
	if role == "service" {
		return nil
	}
	return func(t *Table) *apiError {
		if p.publicTable(t) {
			return nil
		}
		switch t.Kind {
		case kindView:
			if t.Invoker {
				return p.viewGate(cat, role, t, false)
			}
			return &apiError{Status: http.StatusForbidden, Code: "rls_required", Details: map[string]any{"table": t.Name},
				Message: fmt.Sprintf("view %s doesn't run as the caller (security_invoker), so row-level security can't apply: set it, or mark it public", t.Name)}
		case kindTable, kindPart, kindForeign:
			if t.RLS {
				return nil
			}
		}
		return &apiError{Status: http.StatusForbidden, Code: "rls_required", Details: map[string]any{"table": t.Name},
			Message: fmt.Sprintf("%s has no row-level security, so it can't be read with the publishable key: enable it and add policies, or mark the table public", t.Name)}
	}
}

// viewGate checks what a security_invoker view reads (datacat's
// Unprotected and CallsFunctions): every relation without row-level
// security must be a public table (for a read), and a view that calls the
// project's functions passes only when functionGate would.
func (p *project) viewGate(cat *Catalog, role string, t *Table, write bool) *apiError {
	for _, u := range t.Unprotected {
		if write || !p.publicName(u) {
			return &apiError{Status: http.StatusForbidden, Code: "rls_required", Details: map[string]any{"table": t.Name, "reads": u},
				Message: fmt.Sprintf("view %s reads %s, which row-level security doesn't protect, so it can't be used with the publishable key: enable row-level security on %s and add policies, or mark it public", t.Name, u, u)}
		}
	}
	if t.CallsFunctions {
		if reach := cat.Reach[role]; len(reach) > 0 {
			return reachError("view "+t.Name+" calls a function, which", reach, map[string]any{"table": t.Name})
		}
	}
	return nil
}

// functionGate is the RLS gate for a function: one that runs as the
// caller reaches whatever the caller's role may, so anon and user may call
// it only while their role holds no privilege on a relation nothing
// protects (Catalog.Reach; the project's database takes those back when
// the catalog changes, so this is a backstop). A SECURITY DEFINER
// function runs as its owner, which the advisor flags.
func (p *project) functionGate(cat *Catalog, role string, f *datacat.Function) *apiError {
	if role == "service" || f.Definer {
		return nil
	}
	if reach := cat.Reach[role]; len(reach) > 0 {
		return reachError(f.Name+" runs as the caller, who", reach, map[string]any{"function": f.Name})
	}
	return nil
}

func reachError(who string, reach []string, details map[string]any) *apiError {
	details["reaches"] = reach
	return &apiError{Status: http.StatusForbidden, Code: "rls_required", Details: details,
		Message: fmt.Sprintf("%s can reach %s, which row-level security doesn't protect, so it can't be called with the publishable key: enable row-level security on %s, or revoke the request roles' privileges on it", who, reach[0], reach[0])}
}

func (c *call) apiFail(e *apiError) {
	if e.RetryAfter > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	c.json(e.Status, map[string]Error{"error": {Code: e.Code, Message: e.Message, Details: e.Details, RequestID: c.id}})
}

// data routes /data/v1/....
func (e *Edge) data(c *call, req Request) {
	rest := strings.TrimPrefix(c.r.URL.EscapedPath(), "/data/v1/")
	segs := strings.Split(rest, "/")
	for i, s := range segs {
		u, err := url.PathUnescape(s)
		if err != nil {
			c.apiFail(badRequest("invalid_path", "the path is malformed"))
			return
		}
		segs[i] = u
	}
	m := c.r.Method
	switch {
	case len(segs) == 2 && segs[0] == "rpc" && segs[1] != "":
		e.rpc(c, req, segs[1])
	case len(segs) == 1 && segs[0] == "batch" && m == http.MethodPost:
		e.batch(c, req)
	case len(segs) == 1 && segs[0] != "" && segs[0] != "openapi.json" && (m == http.MethodPost || m == http.MethodPatch || m == http.MethodDelete):
		e.write(c, req, segs[0], nil)
	case len(segs) == 2 && segs[1] != "" && segs[1] != "query" && (m == http.MethodPatch || m == http.MethodDelete):
		pk := segs[1]
		e.write(c, req, segs[0], &pk)
	case len(segs) == 1 && segs[0] == "openapi.json" && m == http.MethodGet:
		e.openAPI(c, req)
	case len(segs) == 1 && segs[0] != "" && m == http.MethodGet:
		q, err := queryFromURL(c.r.URL.Query())
		if err != nil {
			c.apiFail(asAPIError(err))
			return
		}
		e.read(c, req, segs[0], q, false)
	case len(segs) == 2 && segs[1] == "query" && m == http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(c.r.Body, maxBodyBytes+1))
		if err != nil || len(body) > maxBodyBytes {
			c.apiFail(&apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "a query body is at most 1 MB"})
			return
		}
		q, err := queryFromJSON(body)
		if err != nil {
			c.apiFail(asAPIError(err))
			return
		}
		e.read(c, req, segs[0], q, false)
	case len(segs) == 2 && segs[1] != "" && m == http.MethodGet:
		q, err := queryFromURL(c.r.URL.Query())
		if err != nil {
			c.apiFail(asAPIError(err))
			return
		}
		pk := segs[1]
		q.PKValue = &pk
		e.read(c, req, segs[0], q, true)
	case m == http.MethodPut:
		c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "use POST to insert or upsert, PATCH to update")
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
	}
}

func asAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	return badRequest("bad_request", "%s", err.Error())
}

var queryParams = map[string]bool{"select": true, "where": true, "or": true, "order": true, "limit": true, "offset": true,
	"cursor": true, "count": true, "apikey": true}

func queryFromURL(v url.Values) (Query, error) {
	q := Query{}
	for k := range v {
		if !queryParams[k] {
			return q, badRequest("unknown_parameter", "unknown parameter %q", k)
		}
	}
	var err error
	if q.Select, err = ParseSelect(v.Get("select"), 0); err != nil {
		return q, err
	}
	for _, w := range v["where"] {
		f, err := ParseCondition(w)
		if err != nil {
			return q, err
		}
		q.Where.And = append(q.Where.And, f)
	}
	for _, o := range v["or"] {
		f, err := ParseOr(o)
		if err != nil {
			return q, err
		}
		q.Where.And = append(q.Where.And, f)
	}
	if q.Order, err = ParseOrder(strings.Join(v["order"], ",")); err != nil {
		return q, err
	}
	if s := v.Get("limit"); s != "" {
		if q.Limit, err = strconv.Atoi(s); err != nil {
			return q, badRequest("invalid_limit", "limit is a number")
		}
	}
	if s := v.Get("offset"); s != "" {
		if q.Offset, err = strconv.Atoi(s); err != nil {
			return q, badRequest("invalid_offset", "offset is a number")
		}
	}
	q.Cursor = v.Get("cursor")
	q.Count = v.Get("count")
	if q.Count != "" && q.Count != "exact" && q.Count != "estimated" {
		return q, badRequest("invalid_count", "count is exact or estimated")
	}
	return q, nil
}

// jsonQuery is POST /data/v1/{table}/query's body.
type jsonQuery struct {
	Select string          `json:"select"`
	Where  json.RawMessage `json:"where"`
	Order  json.RawMessage `json:"order"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
	Cursor string          `json:"cursor"`
	Count  string          `json:"count"`
}

func queryFromJSON(body []byte) (Query, error) {
	var jq jsonQuery
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&jq); err != nil {
		return Query{}, badRequest("invalid_body", "the body isn't a query: %v", err)
	}
	q := Query{Limit: jq.Limit, Offset: jq.Offset, Cursor: jq.Cursor, Count: jq.Count}
	var err error
	if q.Select, err = ParseSelect(jq.Select, 0); err != nil {
		return q, err
	}
	if len(jq.Where) > 0 && string(jq.Where) != "null" {
		if q.Where, err = filterFromJSON(jq.Where, 0); err != nil {
			return q, err
		}
	}
	if len(jq.Order) > 0 && string(jq.Order) != "null" {
		var s string
		if json.Unmarshal(jq.Order, &s) == nil {
			if q.Order, err = ParseOrder(s); err != nil {
				return q, err
			}
		} else {
			var list []struct {
				Column    string `json:"column"`
				Direction string `json:"direction"`
			}
			if err := json.Unmarshal(jq.Order, &list); err != nil {
				return q, badRequest("invalid_order", "order is \"col:desc,…\" or [{\"column\":…,\"direction\":…}]")
			}
			for _, o := range list {
				parsed, err := ParseOrder(o.Column + ":" + o.Direction)
				if err != nil {
					return q, err
				}
				q.Order = append(q.Order, parsed...)
			}
		}
	}
	if q.Count != "" && q.Count != "exact" && q.Count != "estimated" {
		return q, badRequest("invalid_count", "count is exact or estimated")
	}
	return q, nil
}

// filterFromJSON reads {"and":[…]}, {"or":[…]}, {"not":…} or
// {"column":…,"op":…,"value":…}.
func filterFromJSON(raw json.RawMessage, depth int) (Filter, error) {
	if depth > 8 {
		return Filter{}, badRequest("invalid_filter", "filters nest at most 8 levels")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Filter{}, badRequest("invalid_filter", "a filter is an object")
	}
	group := func(key string) ([]Filter, error) {
		var list []json.RawMessage
		if err := json.Unmarshal(m[key], &list); err != nil {
			return nil, badRequest("invalid_filter", "%s takes a list of filters", key)
		}
		var out []Filter
		for _, x := range list {
			f, err := filterFromJSON(x, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	}
	switch {
	case m["and"] != nil:
		list, err := group("and")
		return Filter{And: list}, err
	case m["or"] != nil:
		list, err := group("or")
		return Filter{Or: list}, err
	case m["not"] != nil:
		f, err := filterFromJSON(m["not"], depth+1)
		return Filter{Not: &f}, err
	}
	var leaf struct {
		Column string          `json:"column"`
		Op     string          `json:"op"`
		Value  json.RawMessage `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&leaf); err != nil {
		return Filter{}, badRequest("invalid_filter", "a condition is {\"column\":…,\"op\":…,\"value\":…}")
	}
	if !ops[leaf.Op] {
		return Filter{}, badRequest("invalid_filter", "unknown operator %q", leaf.Op)
	}
	col, path, err := parsePath(leaf.Column)
	if err != nil {
		return Filter{}, err
	}
	f := Filter{Column: col, Path: path, Op: leaf.Op}
	text := func(v json.RawMessage) (string, bool) {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s, false
		}
		t := strings.TrimSpace(string(v))
		return t, t == "null"
	}
	if leaf.Op == "in" {
		var list []json.RawMessage
		if err := json.Unmarshal(leaf.Value, &list); err != nil {
			return Filter{}, badRequest("invalid_filter", "in takes a list")
		}
		for _, x := range list {
			s, _ := text(x)
			f.Values = append(f.Values, s)
		}
		return f, nil
	}
	f.Value, f.Null = text(leaf.Value)
	if leaf.Op == "is" {
		if f.Null {
			f.Value = "null"
		}
		if f.Value != "null" && f.Value != "true" && f.Value != "false" {
			return Filter{}, badRequest("invalid_filter", "is takes null, true or false")
		}
		f.Null = false
	}
	return f, nil
}

// cost is the estimated cost of a statement shape, explained once per
// catalog version.
func (e *Edge) cost(ctx context.Context, tx pgx.Tx, st *catalogState, sql string, args []any) (float64, error) {
	st.mu.Lock()
	c, ok := st.shapes[sql]
	st.mu.Unlock()
	if ok {
		return c, nil
	}
	var plan []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
			Rows      float64 `json:"Plan Rows"`
		} `json:"Plan"`
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return 0, err
	}
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) == 0 {
		return 0, fmt.Errorf("unreadable plan")
	}
	c = plan[0].Plan.TotalCost
	st.mu.Lock()
	if len(st.shapes) > 2000 {
		st.shapes = map[string]float64{}
	}
	st.shapes[sql] = c
	st.mu.Unlock()
	return c, nil
}

// read runs a read: a list, or one row by its key.
func (e *Edge) read(c *call, req Request, name string, q Query, one bool) {
	// The cache (V4.1 §10): a hit needs the catalog this edge last read.
	caching := len(c.p.cfg.Settings.CacheTTLSeconds) > 0
	var gen uint64
	if caching {
		gen = e.cache.generation(c.p.cfg.Ref)
		if fp := c.p.catalog.fingerprint(); fp != "" {
			if t := c.p.catalog.table(name); t != nil && c.cacheable(req, cacheName(t.Schema, t.Name, false)) > 0 &&
				e.serveCached(c, cacheKey(c.p.cfg.Ref, c.p.cfg.Version, fp, c.r.URL.Path, c.r.URL.Query())) {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+countTimeout+3*time.Second)
	defer cancel()
	var (
		rows   []json.RawMessage
		last   []*string
		count  *int64
		stmt   statement
		failed *apiError
		size   int
		tbl    *Table
		fp     string
	)
	err := e.WithRequest(ctx, c.p, req, func(tx pgx.Tx) error {
		cat, st, err := e.catalog(ctx, c.p, tx)
		if err != nil {
			return err
		}
		fp = cat.Fingerprint
		t := cat.Find(name)
		tbl = t
		if t == nil {
			failed = &apiError{Status: http.StatusNotFound, Code: "unknown_table",
				Message: fmt.Sprintf("no table or view %q in the exposed schemas (%s)", name, strings.Join(cat.Schemas, ", "))}
			return nil
		}
		g := c.p.gateFor(cat, req.Role)
		if g != nil {
			if ge := g(t); ge != nil {
				failed = ge
				return nil
			}
		}
		if one {
			q.Limit, q.Order, q.Cursor, q.Offset, q.Count = 1, nil, "", 0, ""
		}
		b := &builder{cat: cat, gate: g}
		stmt, err = b.read(t, q)
		if err != nil {
			if ae := asAPIError(err); ae != nil {
				failed = ae
				return nil
			}
			return err
		}
		if limit := c.p.cfg.Settings.MaxQueryCost; limit > 0 {
			cost, err := e.cost(ctx, tx, st, stmt.SQL, stmt.Args)
			if err != nil {
				return err
			}
			if cost > limit {
				failed = &apiError{Status: http.StatusBadRequest, Code: "query_too_expensive",
					Message: "the query's estimated cost is over the project's limit: add an index, a filter or a smaller limit",
					Details: map[string]any{"estimated_cost": cost, "limit": limit}}
				return nil
			}
		}
		res, err := tx.Query(ctx, stmt.SQL, stmt.Args...)
		if err != nil {
			return err
		}
		for res.Next() {
			vals := res.RawValues()
			if vals[0] == nil {
				continue
			}
			size += len(vals[0]) + 1
			if size > maxResultBytes {
				res.Close()
				failed = &apiError{Status: http.StatusRequestEntityTooLarge, Code: "result_too_large",
					Message: "the result is over 10 MB: select fewer columns or a smaller limit"}
				return nil
			}
			rows = append(rows, append(json.RawMessage(nil), vals[0]...))
			last = last[:0]
			for _, v := range vals[1:] {
				if v == nil {
					last = append(last, nil)
				} else {
					s := string(v)
					last = append(last, &s)
				}
			}
		}
		res.Close()
		if err := res.Err(); err != nil {
			return err
		}
		switch q.Count {
		case "exact":
			n, err := countExact(ctx, tx, stmt)
			if err != nil {
				return err
			}
			count = n
		case "estimated":
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+stmt.CountSQL, stmt.CountArgs...).Scan(&raw); err != nil {
				return err
			}
			var plan []struct {
				Plan struct {
					Plans []struct {
						Rows float64 `json:"Plan Rows"`
					} `json:"Plans"`
					Rows float64 `json:"Plan Rows"`
				} `json:"Plan"`
			}
			if json.Unmarshal(raw, &plan) == nil && len(plan) > 0 {
				est := plan[0].Plan.Rows
				if len(plan[0].Plan.Plans) > 0 {
					est = plan[0].Plan.Plans[0].Rows // under the aggregate
				}
				n := int64(est)
				count = &n
			}
		}
		return nil
	})
	if err != nil {
		e.dataDBError(c, err)
		return
	}
	if failed != nil {
		c.apiFail(failed)
		return
	}
	var buf bytes.Buffer
	if one {
		if len(rows) == 0 {
			c.fail(http.StatusNotFound, "not_found", "no row with that key")
			return
		}
		buf.WriteString(`{"data":`)
		buf.Write(rows[0])
		buf.WriteString("}\n")
	} else {
		buf.WriteString(`{"data":[`)
		for i, r := range rows {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(r)
		}
		buf.WriteByte(']')
		if len(rows) == stmt.Limit && len(stmt.Keys) > 0 {
			t := c.p.catalog.table(name)
			if t != nil {
				cur, _ := json.Marshal(encodeCursor(t, stmt.Keys, last))
				buf.WriteString(`,"next_cursor":`)
				buf.Write(cur)
			}
		}
		if count != nil {
			buf.WriteString(`,"count":` + strconv.FormatInt(*count, 10))
		} else if q.Count == "exact" {
			buf.WriteString(`,"count":null`)
		}
		buf.WriteString("}\n")
	}
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if caching && tbl != nil {
		if ttl := c.cacheable(req, cacheName(tbl.Schema, tbl.Name, false)); ttl > 0 {
			e.cacheStore(c, cacheKey(c.p.cfg.Ref, c.p.cfg.Version, fp, c.r.URL.Path, c.r.URL.Query()), buf.Bytes(), ttl, stmt.Tables, gen)
		}
	}
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(buf.Bytes())
}

// countExact counts the filtered rows under a short timeout; on timeout
// the count is nil (V4 §3.2 "Exact counts are capped by a timeout").
func countExact(ctx context.Context, tx pgx.Tx, stmt statement) (*int64, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT xb_count"); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)", strconv.FormatInt(countTimeout.Milliseconds(), 10)); err != nil {
		return nil, err
	}
	var n int64
	err := tx.QueryRow(ctx, stmt.CountSQL, stmt.CountArgs...).Scan(&n)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "57014" {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT xb_count"); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// table is a table of the cached catalog by its URL name.
func (st *catalogState) table(name string) *Table {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cat == nil {
		return nil
	}
	return st.cat.Find(name)
}

// invalidate drops the cached catalog (a query named something the
// catalog didn't have: it changed).
func (st *catalogState) invalidate() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.cat = nil
}

// pgErrors maps Postgres error classes to the data API's codes (V4 §3.5).
var pgErrors = map[string]struct {
	status int
	code   string
}{
	"42501": {http.StatusForbidden, "permission_denied"},
	"42P01": {http.StatusNotFound, "unknown_table"},
	"42703": {http.StatusBadRequest, "unknown_column"},
	"42883": {http.StatusBadRequest, "unsupported_operator"},
	"42804": {http.StatusBadRequest, "type_mismatch"},
	"22P02": {http.StatusBadRequest, "invalid_value"},
	"22007": {http.StatusBadRequest, "invalid_value"},
	"22008": {http.StatusBadRequest, "invalid_value"},
	"22003": {http.StatusBadRequest, "invalid_value"},
	"22023": {http.StatusBadRequest, "invalid_value"},
	"2201B": {http.StatusBadRequest, "invalid_value"},
	"22025": {http.StatusBadRequest, "invalid_value"},
	"23502": {http.StatusUnprocessableEntity, "not_null_violation"},
	"23503": {http.StatusConflict, "foreign_key_violation"},
	"23505": {http.StatusConflict, "unique_violation"},
	"23514": {http.StatusUnprocessableEntity, "check_violation"},
	"25006": {http.StatusForbidden, "read_only"},
	"57014": {http.StatusGatewayTimeout, "statement_timeout"},
}

func (e *Edge) dataDBError(c *call, err error) {
	if ae := pgAPIError(c, err); ae != nil {
		c.apiFail(ae)
		return
	}
	e.dbError(c, err)
}

// pgAPIError maps a Postgres error the data API explains to the caller (a
// constraint, a policy, a bad value) to an API error, and nil for the rest.
func pgAPIError(c *call, err error) *apiError {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return nil
	}
	if pe.Code == "42P01" || pe.Code == "42703" {
		c.p.catalog.invalidate()
	}
	m, ok := pgErrors[pe.Code]
	if !ok {
		return nil
	}
	return &apiError{Status: m.status, Code: m.code, Message: pe.Message, Details: map[string]any{"pg_code": pe.Code}}
}
