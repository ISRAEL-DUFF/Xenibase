package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// Functions (V4 §3.4): POST /data/v1/rpc/{function} with named arguments
// as JSON; GET for STABLE and IMMUTABLE ones, arguments as query
// parameters. Set-returning functions take select, where, order and limit
// on their output. The function runs as the caller, with the caller's
// EXECUTE privilege and row-level security.

var rpcOutputParams = map[string]bool{"select": true, "where": true, "or": true, "order": true, "limit": true, "offset": true, "apikey": true}

// argValue is an argument as SQL: a parameter cast to the argument's
// type. Arrays come as JSON arrays, json and jsonb as JSON.
func (b *builder) argValue(a datacat.Arg, raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" {
		return "NULL::" + a.Type
	}
	switch {
	case a.TypName == "json" || a.TypName == "jsonb":
		return b.param(string(raw)) + "::" + a.Type
	case a.Category == 'A' && len(raw) > 0 && raw[0] == '[':
		return "ARRAY(SELECT json_array_elements_text(" + b.param(string(raw)) + "::json))::" + a.Type
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw) // a number or a boolean
	}
	return b.param(s) + "::" + a.Type
}

// pickFunction chooses the overload whose argument names cover args and
// which gets every argument without a default.
func pickFunction(fs []*datacat.Function, args map[string]json.RawMessage) (*datacat.Function, error) {
	var match []*datacat.Function
outer:
	for _, f := range fs {
		names := map[string]bool{}
		for _, a := range f.Args {
			if a.Name == "" {
				continue outer // positional arguments only: not callable by name
			}
			names[a.Name] = true
			if _, ok := args[a.Name]; !ok && !a.HasDefault {
				continue outer
			}
		}
		for k := range args {
			if !names[k] {
				continue outer
			}
		}
		match = append(match, f)
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		var given []string
		for k := range args {
			given = append(given, k)
		}
		sort.Strings(given)
		return nil, &apiError{Status: http.StatusBadRequest, Code: "no_matching_function",
			Message: fmt.Sprintf("%s takes no such arguments (%s): give each argument without a default by name", fs[0].Name, strings.Join(given, ", "))}
	}
	return nil, &apiError{Status: http.StatusBadRequest, Code: "ambiguous_function",
		Message: fmt.Sprintf("more than one %s matches these arguments", fs[0].Name)}
}

func (e *Edge) rpc(c *call, req Request, name string) {
	args := map[string]json.RawMessage{}
	q := c.r.URL.Query()
	output := url.Values{}
	var key *idempotency
	switch c.r.Method {
	case http.MethodPost:
		body, ok := readBody(c)
		if !ok {
			return
		}
		var ae *apiError
		if key, ae = idempotencyOf(c, req, body); ae != nil {
			c.apiFail(ae)
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &args); err != nil {
				c.apiFail(badRequest("invalid_body", "arguments are a JSON object of name: value"))
				return
			}
		}
		output = q
	case http.MethodGet:
		for k, vs := range q {
			if rpcOutputParams[k] {
				output[k] = vs
				continue
			}
			v, _ := json.Marshal(vs[0])
			args[k] = v
		}
	default:
		c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "call a function with POST, or GET when it is stable")
		return
	}
	for k := range output {
		if !rpcOutputParams[k] {
			c.apiFail(badRequest("unknown_parameter", "unknown parameter %q", k))
			return
		}
	}
	oq, err := queryFromURL(output)
	if err != nil {
		c.apiFail(asAPIError(err))
		return
	}
	// The cache (V4.1 §10), for stable functions called with GET.
	caching := len(c.p.cfg.Settings.CacheTTLSeconds) > 0 && c.r.Method == http.MethodGet
	var gen uint64
	if caching {
		gen = e.cache.generation(c.p.cfg.Ref)
		if fp := c.p.catalog.fingerprint(e.cfg.CatalogEvery); fp != "" {
			if f := c.p.catalog.function(name); f != nil && c.cacheable(req, cacheName(f.Schema, f.Name, true)) > 0 &&
				e.serveCached(c, cacheKey(c.p.cfg.Ref, c.p.cfg.Version, fp, c.r.URL.Path, q)) {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+3*time.Second)
	defer cancel()
	var out []byte
	var failed *apiError
	var fn *datacat.Function
	var fp string
	err = e.WithRequest(ctx, c.p, req, func(tx pgx.Tx) error {
		if err := key.claim(ctx, tx); err != nil {
			return err
		}
		cat, _, err := e.catalog(ctx, c.p, tx)
		if err != nil {
			return err
		}
		fp = cat.Fingerprint
		fs := cat.FindFunction(name)
		if len(fs) == 0 {
			failed = &apiError{Status: http.StatusNotFound, Code: "unknown_function",
				Message: fmt.Sprintf("no function %q in the exposed schemas (%s)", name, strings.Join(cat.Schemas, ", "))}
			return nil
		}
		f, err := pickFunction(fs, args)
		if err != nil {
			failed = asAPIError(err)
			return nil
		}
		fn = f
		if ge := c.p.functionGate(cat, req.Role, f); ge != nil {
			failed = ge
			return nil
		}
		if c.r.Method == http.MethodGet && f.Volatile == 'v' {
			failed = &apiError{Status: http.StatusMethodNotAllowed, Code: "volatile_function",
				Message: fmt.Sprintf("%s may change data (it isn't STABLE or IMMUTABLE): call it with POST", f.Name)}
			return nil
		}
		b := &builder{cat: cat, gate: c.p.gateFor(cat, req.Role)}
		var named []string
		for _, a := range f.Args {
			if raw, ok := args[a.Name]; ok {
				named = append(named, qi(a.Name)+" => "+b.argValue(a, raw))
			}
		}
		call := qi(f.Schema) + "." + qi(f.Name) + "(" + strings.Join(named, ", ") + ")"
		sql, list, err := b.rpcSQL(f, call, oq)
		if err != nil {
			failed = asAPIError(err)
			return nil
		}
		// A set-returning function's rows go through the cost guard, which
		// sees through a SQL function the planner inlines.
		if limit := c.p.cfg.Settings.MaxQueryCost; limit > 0 && f.RetSet {
			est, err := cost(ctx, tx, sql, b.args)
			if err != nil {
				return err
			}
			if est > limit {
				failed = tooExpensive(est, limit)
				return nil
			}
		}
		rows, err := tx.Query(ctx, sql, b.args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var buf bytes.Buffer
		buf.WriteString(`{"data":`)
		if list {
			buf.WriteByte('[')
		}
		n, size := 0, 0
		for rows.Next() {
			v := rows.RawValues()[0]
			size += len(v)
			if size > maxResultBytes {
				failed = &apiError{Status: http.StatusRequestEntityTooLarge, Code: "result_too_large", Message: "the result is over 10 MB"}
				return nil
			}
			if n > 0 {
				buf.WriteByte(',')
			}
			if v == nil {
				buf.WriteString("null")
			} else {
				buf.Write(v)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if list {
			buf.WriteByte(']')
		} else if n == 0 {
			buf.WriteString("null")
		}
		buf.WriteString("}\n")
		out = buf.Bytes()
		return key.store(ctx, tx, http.StatusOK, out)
	})
	if err != nil {
		var rp *replayed
		var ae *apiError
		switch {
		case errors.As(err, &rp):
			c.writeReplay(rp)
		case errors.As(err, &ae):
			c.apiFail(ae)
		default:
			e.dataDBError(c, err)
		}
		return
	}
	if failed != nil {
		c.apiFail(failed)
		return
	}
	if c.r.Method == http.MethodPost {
		e.dropWritten(c, nil, true) // it may have written anything
	}
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if caching && fn != nil {
		if ttl := c.cacheable(req, cacheName(fn.Schema, fn.Name, true)); ttl > 0 {
			e.cacheStore(c, cacheKey(c.p.cfg.Ref, c.p.cfg.Version, fp, c.r.URL.Path, q), out, ttl, []string{rpcTag}, gen)
		}
	}
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(out)
}

// rpcSQL is the call's statement; list says the result is an array.
func (b *builder) rpcSQL(f *datacat.Function, call string, q Query) (string, bool, error) {
	if f.Columns == nil {
		if countFilters(q.Where) > 0 || len(q.Order) > 0 {
			return "", false, badRequest("invalid_filter", "%s returns values, not rows: filters and order don't apply", f.Name)
		}
		if f.RetSet {
			limit := q.Limit
			if limit == 0 {
				limit = maxLimit
			}
			return "SELECT to_json(v) FROM " + call + " AS v LIMIT " + strconv.Itoa(min(limit, maxLimit)), true, nil
		}
		return "SELECT to_json(" + call + ")", false, nil
	}
	// Rows: the output as a relation the read builder can filter.
	t := &Table{Schema: f.Schema, Name: f.Name, Kind: kindView, ByName: map[string]*Column{}}
	for _, col := range f.Columns {
		t.Columns = append(t.Columns, col)
		t.ByName[col.Name] = col
	}
	alias := b.nextAlias()
	cols, err := b.selectList(t, alias, q.Select, 0)
	if err != nil {
		return "", false, err
	}
	sql := "SELECT (SELECT row_to_json(r) FROM (SELECT " + strings.Join(cols, ", ") + ") r) FROM " + call + " AS " + alias
	w, err := b.where(t, alias, q.Where)
	if err != nil {
		return "", false, err
	}
	if w != "" {
		sql += " WHERE " + w
	}
	if len(q.Order) > 0 {
		var ord []string
		for _, o := range q.Order {
			if t.Col(o.Column) == nil {
				return "", false, &apiError{Status: 400, Code: "unknown_column", Message: fmt.Sprintf("%s returns no column %q", f.Name, o.Column)}
			}
			s := alias + "." + qi(o.Column)
			if o.Desc {
				s += " DESC"
			}
			ord = append(ord, s)
		}
		sql += " ORDER BY " + strings.Join(ord, ", ")
	}
	if !f.RetSet {
		return sql, false, nil
	}
	limit := q.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return "", false, badRequest("invalid_limit", "limit is 1 to %d", maxLimit)
	}
	sql += " LIMIT " + strconv.Itoa(limit)
	if q.Offset > 0 {
		sql += " OFFSET " + strconv.Itoa(min(q.Offset, maxOffset))
	}
	return sql, true, nil
}
