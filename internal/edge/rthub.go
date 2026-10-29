package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// A project's realtime on this edge process: its connections' channels,
// one LISTEN connection to its database while any are joined (through a
// session-mode pooler; without one the outbox is polled), and the outbox
// cursor (§6.2).

const (
	// rtPoll is how often the outbox is read without a notification (the
	// fallback, and the cadence without a LISTEN connection).
	rtPoll = time.Second
	// rtLinger keeps a project's LISTEN connection this long after its last
	// channel leaves (a reconnecting client comes straight back).
	rtLinger = 30 * time.Second
	// rtBatch is the outbox rows read at a time.
	rtBatch = 1000
	// rtTablesTTL is how long the list of tables with realtime is cached.
	rtTablesTTL = 3 * time.Second
	// rtHelloEvery announces this process to the project's other edge
	// processes; one not heard for rtPeerExpiry is gone (§6.5).
	rtHelloEvery = 10 * time.Second
	rtPeerExpiry = 35 * time.Second

	// rtMeterEvery is how often connection time is counted.
	rtMeterEvery = 10 * time.Second

	chanChanges   = "xb_realtime"
	chanBroadcast = "xb_realtime_bc"
)

type rtHub struct {
	e   *Edge
	ref string

	conns atomic.Int64
	msgs  atomic.Int64 // messages to and from clients, since the last report

	meterMu   sync.Mutex
	meteredAt time.Time

	mu        sync.Mutex
	channels  map[*rtChannel]struct{}
	byTopic   map[string]map[*rtChannel]struct{}
	subs      map[string]map[*rtSub]struct{} // "schema.table"
	listening bool
	emptyAt   time.Time
	// ready is closed once the listener reads the outbox from a point
	// after every channel joined so far: a join waits for it before
	// answering, so no change committed after "subscribed" is missed.
	ready chan struct{}

	tablesMu sync.Mutex
	tables   map[string][]string // "schema.table" -> primary key columns
	tablesAt time.Time

	// The outbox cursor (listener goroutine only): every transaction
	// below lowWater is done; delivered are those at or above it already
	// read, to their last row.
	lowWater  uint64
	delivered map[uint64]int64

	rateMu      sync.Mutex
	rateWindow  time.Time
	rateCount   int
	rateTripped map[*rtSub]struct{}

	// Broadcast and presence across processes (rtbroadcast.go).
	bc rtBroadcastState
}

// hub is ref's realtime state, created on first use.
func (e *Edge) hub(ref string) *rtHub {
	e.rtMu.Lock()
	defer e.rtMu.Unlock()
	if e.hubs == nil {
		e.hubs = map[string]*rtHub{}
	}
	h := e.hubs[ref]
	if h == nil {
		h = &rtHub{e: e, ref: ref, channels: map[*rtChannel]struct{}{}, byTopic: map[string]map[*rtChannel]struct{}{},
			subs: map[string]map[*rtSub]struct{}{}}
		h.bc.init()
		h.meteredAt = time.Now()
		e.hubs[ref] = h
	}
	return h
}

// admit counts a new connection, false when that is past max (0: none).
func (h *rtHub) admit(limit int) bool {
	n := h.conns.Add(1)
	return limit <= 0 || n <= int64(limit)
}

func (h *rtHub) release()        { h.conns.Add(-1) }
func (h *rtHub) counted(n int64) { h.msgs.Add(n) }

func (h *rtHub) join(ch *rtChannel) {
	h.mu.Lock()
	h.channels[ch] = struct{}{}
	if h.byTopic[ch.topic] == nil {
		h.byTopic[ch.topic] = map[*rtChannel]struct{}{}
	}
	h.byTopic[ch.topic][ch] = struct{}{}
	for _, s := range ch.subs {
		k := s.schema + "." + s.table
		if h.subs[k] == nil {
			h.subs[k] = map[*rtSub]struct{}{}
		}
		h.subs[k][s] = struct{}{}
	}
	start := !h.listening
	h.listening = true
	if start {
		h.ready = make(chan struct{})
	}
	h.mu.Unlock()
	if start {
		go h.listen()
	}
}

// waitReady waits until the listener reads the outbox (or ctx ends).
func (h *rtHub) waitReady(ctx context.Context) {
	h.mu.Lock()
	ready := h.ready
	h.mu.Unlock()
	if ready == nil {
		return
	}
	select {
	case <-ready:
	case <-ctx.Done():
	}
}

// markReady opens ready for this connection's cursor; a reconnect starts
// a new one.
func (h *rtHub) markReady() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ready != nil {
		select {
		case <-h.ready:
		default:
			close(h.ready)
		}
	}
}

func (h *rtHub) leave(ch *rtChannel) {
	h.mu.Lock()
	if _, ok := h.channels[ch]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.channels, ch)
	if t := h.byTopic[ch.topic]; t != nil {
		delete(t, ch)
		if len(t) == 0 {
			delete(h.byTopic, ch.topic)
		}
	}
	for _, s := range ch.subs {
		k := s.schema + "." + s.table
		if m := h.subs[k]; m != nil {
			delete(m, s)
			if len(m) == 0 {
				delete(h.subs, k)
			}
		}
	}
	if len(h.channels) == 0 {
		h.emptyAt = time.Now()
	}
	h.mu.Unlock()
	h.untrackChannel(ch)
}

// idle reports a hub with no channels for rtLinger; it stops listening.
func (h *rtHub) idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.channels) == 0 && time.Since(h.emptyAt) > rtLinger {
		h.listening = false
		return true
	}
	return false
}

func (h *rtHub) hasSubs() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// enabledTables are the project's tables with realtime on.
func (h *rtHub) enabledTables(ctx context.Context, p *project) (map[string]bool, error) {
	t, err := h.tableKeys(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(t))
	for k := range t {
		out[k] = true
	}
	return out, nil
}

func (h *rtHub) tableKeys(ctx context.Context, p *project) (map[string][]string, error) {
	h.tablesMu.Lock()
	defer h.tablesMu.Unlock()
	if h.tables != nil && time.Since(h.tablesAt) < rtTablesTTL {
		return h.tables, nil
	}
	pool, err := h.e.dbPool(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT schema_name, table_name, pk_columns FROM xb_realtime.tables`)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for rows.Next() {
		var s, t string
		var pk []string
		if err := rows.Scan(&s, &t, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		out[s+"."+t] = pk
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	h.tables, h.tablesAt = out, time.Now()
	return out, nil
}

// forgetTables drops the cached list of tables with realtime on.
func (h *rtHub) forgetTables() {
	h.tablesMu.Lock()
	h.tables = nil
	h.tablesMu.Unlock()
}

// listen holds the project's LISTEN connection and reads its outbox until
// the hub has been idle for rtLinger.
func (h *rtHub) listen() {
	h.e.rtMu.Lock()
	life := h.e.life
	h.e.rtMu.Unlock()
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithCancel(life)
	defer cancel()
	backoff := time.Second
	first := true
	for {
		if h.idle() {
			return
		}
		if ctx.Err() != nil {
			h.mu.Lock()
			h.listening = false
			h.mu.Unlock()
			return
		}
		p := h.e.lookup(h.ref)
		if p == nil {
			h.closeAll("the project's backend services are off")
			h.mu.Lock()
			h.listening = false
			h.mu.Unlock()
			return
		}
		conn, err := h.e.sessionConn(ctx, p)
		if err != nil && !errors.Is(err, errNoSession) {
			h.e.cfg.Log.Warn("realtime listen", "ref", h.ref, "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if !first {
			// Changes may have been missed while disconnected.
			h.resyncAll()
		}
		first = false
		err = h.serve(ctx, conn)
		if conn != nil {
			_ = conn.Close(context.Background())
		}
		if err == nil {
			return // idle
		}
		h.e.cfg.Log.Warn("realtime connection lost", "ref", h.ref, "err", err)
	}
}

var errNoSession = errors.New("no session-mode pooler")

// sessionConn is a session-mode connection as the project's edge login, for
// LISTEN; errNoSession when the feed gave no session pooler.
func (e *Edge) sessionConn(ctx context.Context, p *project) (*pgx.Conn, error) {
	host, port := p.cfg.SessionHost, p.cfg.SessionPort
	if e.cfg.SessionAddr != "" {
		h, ps, err := net.SplitHostPort(e.cfg.SessionAddr)
		if err != nil {
			return nil, err
		}
		host = h
		port, _ = strconv.Atoi(ps)
	}
	if host == "" || port == 0 {
		return nil, errNoSession
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pgx.Connect(cctx, e.poolerURL(p.cfg.EdgeUser, p.cfg.Password, host, port, p.cfg.Database, "xenibase-edge realtime"))
}

// querier is a connection or pool the outbox is read through.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// serve reads the outbox on each notification (or every rtPoll), relays
// broadcasts, and returns nil once idle or an error when the connection
// fails.
func (h *rtHub) serve(ctx context.Context, conn *pgx.Conn) error {
	var q querier = conn
	if conn == nil {
		p := h.e.lookup(h.ref)
		if p == nil {
			return errors.New("project gone")
		}
		pool, err := h.e.dbPool(ctx, p)
		if err != nil {
			return err
		}
		q = pool
	} else {
		if _, err := conn.Exec(ctx, `LISTEN `+chanChanges+`; LISTEN `+chanBroadcast); err != nil {
			return err
		}
	}
	if err := h.initCursor(ctx, q); err != nil {
		return err
	}
	h.markReady()
	h.bc.listening.Store(conn != nil)
	defer h.bc.listening.Store(false)
	h.hello(ctx, q, true)
	lastHello, lastDrain := time.Now(), time.Now()
	for {
		if h.idle() {
			h.goodbye(ctx, q)
			return nil
		}
		changed := false
		if conn != nil {
			wctx, cancel := context.WithTimeout(ctx, rtPoll)
			n, err := conn.WaitForNotification(wctx)
			cancel()
			switch {
			case err == nil && n.Channel == chanChanges:
				changed = true
			case err == nil && n.Channel == chanBroadcast:
				h.relayIn(ctx, q, n.Payload)
			case err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !pgconn.Timeout(err):
				return err
			}
		} else {
			time.Sleep(rtPoll)
		}
		if changed || time.Since(lastDrain) >= rtPoll {
			if err := h.drain(ctx, q); err != nil {
				if conn != nil && conn.IsClosed() {
					return err
				}
				if ctx.Err() == nil {
					h.e.cfg.Log.Warn("realtime outbox", "ref", h.ref, "err", err)
				}
			}
			lastDrain = time.Now()
		}
		if time.Since(lastHello) >= rtHelloEvery {
			h.hello(ctx, q, false)
			h.expirePeers()
			lastHello = time.Now()
		}
	}
}

func (h *rtHub) initCursor(ctx context.Context, q querier) error {
	var snap string
	if err := q.QueryRow(ctx, `SELECT pg_current_snapshot()::text`).Scan(&snap); err != nil {
		return err
	}
	xmin, err := snapshotXmin(snap)
	if err != nil {
		return err
	}
	h.lowWater, h.delivered = xmin, map[uint64]int64{}
	return nil
}

// snapshotXmin is a pg_snapshot's xmin ("xmin:xmax:xip,…").
func snapshotXmin(snap string) (uint64, error) {
	s, _, _ := strings.Cut(snap, ":")
	return strconv.ParseUint(s, 10, 64)
}

// change is one outbox row.
type change struct {
	id     int64
	xid    uint64
	schema string
	table  string
	op     string
	record map[string]any
	old    map[string]any
	at     time.Time
	pk     string // the row's primary key, for the seen sets
}

// drain reads committed changes past the cursor and delivers them.
func (h *rtHub) drain(ctx context.Context, q querier) error {
	var snap string
	if err := q.QueryRow(ctx, `SELECT pg_current_snapshot()::text`).Scan(&snap); err != nil {
		return err
	}
	xmin, err := snapshotXmin(snap)
	if err != nil {
		return err
	}
	if !h.hasSubs() {
		// Nobody to tell: skip what is there.
		h.lowWater, h.delivered = xmin, map[uint64]int64{}
		return nil
	}
	p := h.e.lookup(h.ref)
	if p == nil {
		return nil
	}
	keys, err := h.tableKeys(ctx, p)
	if err != nil {
		return err
	}
	for {
		xs, ids := make([]string, 0, len(h.delivered)), make([]int64, 0, len(h.delivered))
		for x, i := range h.delivered {
			xs = append(xs, strconv.FormatUint(x, 10))
			ids = append(ids, i)
		}
		rows, err := q.Query(ctx, `SELECT o.id, o.xid::text, o.schema_name, o.table_name, o.op, o.record, o.old_record, o.at
			FROM xb_realtime.outbox o
			WHERE o.xid >= $1::text::xid8 AND pg_visible_in_snapshot(o.xid, $2::pg_snapshot)
			  AND NOT EXISTS (SELECT 1 FROM unnest($3::text[]::xid8[], $4::bigint[]) AS d(x, i) WHERE d.x = o.xid AND o.id <= d.i)
			ORDER BY o.xid, o.id LIMIT $5`, strconv.FormatUint(h.lowWater, 10), snap, xs, ids, rtBatch)
		if err != nil {
			return err
		}
		var batch []change
		for rows.Next() {
			var c change
			var xid string
			var rec, old []byte
			if err := rows.Scan(&c.id, &xid, &c.schema, &c.table, &c.op, &rec, &old, &c.at); err != nil {
				rows.Close()
				return err
			}
			c.xid, _ = strconv.ParseUint(xid, 10, 64)
			c.record, c.old = decodeRow(rec), decodeRow(old)
			src := c.record
			if c.op == "DELETE" {
				src = c.old
			}
			c.pk = pkKey(keys[c.schema+"."+c.table], src)
			batch = append(batch, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range batch {
			if c.id > h.delivered[c.xid] {
				h.delivered[c.xid] = c.id
			}
		}
		low := xmin
		if len(batch) == rtBatch && batch[len(batch)-1].xid < low {
			low = batch[len(batch)-1].xid // the rest of that transaction is still to read
		}
		h.lowWater = low
		for x := range h.delivered {
			if x < low {
				delete(h.delivered, x)
			}
		}
		if len(batch) > 0 {
			h.dropCached(p, batch)
			h.deliver(ctx, p, keys, batch)
		}
		if len(batch) < rtBatch {
			return nil
		}
	}
}

func decodeRow(b []byte) map[string]any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil {
		return nil
	}
	return m
}

// pkKey identifies a row by its primary key values.
func pkKey(cols []string, row map[string]any) string {
	if row == nil || len(cols) == 0 {
		return ""
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		v, ok := row[c]
		if !ok {
			return ""
		}
		parts[i] = jsonText(v)
	}
	return strings.Join(parts, "\x00")
}

// group is subscribers who see the same rows: the same role and claims
// (§6.3).
type group struct {
	req  Request
	subs []*rtSub
}

// fingerprint is what decides a group: the role and the claims, less those
// that differ per token (expiry, issue time, session).
func fingerprint(req Request) string {
	if req.Role != "user" {
		return req.Role
	}
	c := make(map[string]any, len(req.Claims))
	for k, v := range req.Claims {
		switch k {
		case "exp", "iat", "nbf", "jti", "session_id":
			continue
		}
		c[k] = v
	}
	b, _ := json.Marshal(c) // map keys are sorted
	return "user:" + string(b)
}

// deliver sends a batch of changes, in order, to the subscribers allowed to
// see each.
func (h *rtHub) deliver(ctx context.Context, p *project, keys map[string][]string, batch []change) {
	// The subscribers of each table touched, and the rows to check.
	byTable := map[string][]*rtSub{}
	need := map[string][]map[string]any{} // table -> pk objects of inserts and updates
	h.mu.Lock()
	for _, c := range batch {
		k := c.schema + "." + c.table
		if _, ok := byTable[k]; !ok {
			for s := range h.subs[k] {
				byTable[k] = append(byTable[k], s)
			}
			if byTable[k] == nil {
				byTable[k] = []*rtSub{}
			}
		}
		if c.op != "DELETE" && len(byTable[k]) > 0 && c.pk != "" {
			pk := map[string]any{}
			for _, col := range keys[k] {
				pk[col] = c.record[col]
			}
			need[k] = append(need[k], pk)
		}
	}
	h.mu.Unlock()
	if !h.allow(len(batch), byTable) {
		return
	}
	limit := p.cfg.Realtime.MaxGroups
	if limit <= 0 {
		limit = 100
	}
	// visible[sub] is the rows (by key) the subscriber's group can see now,
	// as they read them.
	visible := map[*rtSub]map[string]map[string]any{}
	cols := map[string][]map[string]string{}
	for k, subs := range byTable {
		if len(subs) == 0 {
			continue
		}
		groups := map[string]*group{}
		var order []string
		for _, s := range subs {
			req, expired := s.ch.request()
			if expired {
				s.ch.system("error", evPostgresChanges, "the access token has expired: send a new one")
				go s.ch.c.closeChannel(s.ch)
				continue
			}
			fp := fingerprint(req)
			g := groups[fp]
			if g == nil {
				g = &group{req: req}
				groups[fp] = g
				order = append(order, fp)
			}
			g.subs = append(g.subs, s)
		}
		sort.Strings(order)
		for i, fp := range order {
			g := groups[fp]
			if i >= limit {
				for _, s := range g.subs {
					s.ch.system("ok", evPostgresChanges, "resync")
				}
				continue
			}
			if len(need[k]) == 0 {
				continue
			}
			rows, columns, err := h.visibleRows(ctx, p, g.req, k, keys[k], need[k])
			if err != nil {
				h.e.cfg.Log.Warn("realtime visibility check", "ref", h.ref, "table", k, "role", g.req.Role, "err", err)
				for _, s := range g.subs {
					s.ch.system("ok", evPostgresChanges, "resync")
				}
				continue
			}
			cols[k] = columns
			for _, s := range g.subs {
				visible[s] = rows
			}
		}
	}
	for _, c := range batch {
		k := c.schema + "." + c.table
		// One message per channel: the ids of its subscriptions that match.
		type out struct {
			ch     *rtChannel
			ids    []int64
			record map[string]any
		}
		var outs []*out
		byCh := map[*rtChannel]*out{}
		for _, s := range byTable[k] {
			if s.event != "*" && s.event != c.op {
				if c.op != "DELETE" {
					// Keep the seen set right for a later delete.
					if row, ok := visible[s][c.pk]; ok && row != nil {
						s.seen.add(c.pk)
					}
				}
				continue
			}
			var rec map[string]any
			switch c.op {
			case "DELETE":
				if c.pk == "" || !s.seen.has(c.pk) {
					continue
				}
				s.seen.remove(c.pk)
			default:
				rows, checked := visible[s]
				if !checked {
					continue
				}
				row := rows[c.pk]
				if row == nil {
					s.seen.remove(c.pk)
					continue
				}
				if !s.filter.match(row) {
					continue
				}
				s.seen.add(c.pk)
				rec = row
			}
			o := byCh[s.ch]
			if o == nil {
				o = &out{ch: s.ch, record: rec}
				byCh[s.ch] = o
				outs = append(outs, o)
			}
			o.ids = append(o.ids, s.id)
		}
		for _, o := range outs {
			data := map[string]any{"schema": c.schema, "table": c.table, "commit_timestamp": c.at.UTC().Format(time.RFC3339Nano),
				"type": c.op, "eventType": c.op, "columns": cols[k], "errors": nil}
			if o.record != nil {
				data["record"] = o.record
			} else {
				data["record"] = map[string]any{}
			}
			if c.old != nil {
				data["old_record"] = c.old
			} else {
				data["old_record"] = map[string]any{}
			}
			o.ch.c.send(rtMsg{JoinRef: o.ch.joinRef, Topic: o.ch.topic, Event: evPostgresChanges,
				Payload: rawJSON(map[string]any{"ids": o.ids, "data": data})})
		}
	}
}

// allow applies the project's change rate (§6.3): past it, the window's
// remaining changes aren't delivered and their subscribers are told to
// resync once.
func (h *rtHub) allow(n int, byTable map[string][]*rtSub) bool {
	p := h.e.lookup(h.ref)
	limit := 0
	if p != nil {
		limit = p.cfg.Realtime.MaxChangesPerSecond
	}
	if limit <= 0 {
		return true
	}
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	now := time.Now()
	if now.Sub(h.rateWindow) >= time.Second {
		h.rateWindow, h.rateCount, h.rateTripped = now, 0, map[*rtSub]struct{}{}
	}
	h.rateCount += n
	if h.rateCount <= limit {
		return true
	}
	for _, subs := range byTable {
		for _, s := range subs {
			if _, done := h.rateTripped[s]; done {
				continue
			}
			h.rateTripped[s] = struct{}{}
			s.ch.system("ok", evPostgresChanges, "resync")
		}
	}
	return false
}

// visibleRows reads, as req's role and claims, which of the rows (primary
// key objects) of table k it can see, keyed by primary key, with the
// table's columns for the clients' type conversion.
func (h *rtHub) visibleRows(ctx context.Context, p *project, req Request, k string, pk []string, pks []map[string]any) (map[string]map[string]any, []map[string]string, error) {
	out := map[string]map[string]any{}
	var columns []map[string]string
	arg, err := json.Marshal(pks)
	if err != nil {
		return nil, nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = h.e.WithRequest(qctx, p, req, func(tx pgx.Tx) error {
		cat, _, err := h.e.catalog(qctx, p, tx)
		if err != nil {
			return err
		}
		t := cat.ByName[k]
		if t == nil {
			return fmt.Errorf("%s isn't in an exposed schema", k)
		}
		sql, err := visibilitySQL(t, pk)
		if err != nil {
			return err
		}
		for _, c := range t.Columns {
			columns = append(columns, map[string]string{"name": c.Name, "type": c.TypName})
		}
		rows, err := tx.Query(qctx, sql, string(arg))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return err
			}
			row := decodeRow(b)
			out[pkKey(pk, row)] = row
		}
		return rows.Err()
	})
	return out, columns, err
}

// visibilitySQL selects table t's rows with the given primary keys (a JSON
// array of objects) as the caller sees them.
func visibilitySQL(t *datacat.Table, pk []string) (string, error) {
	lhs := make([]string, len(pk))
	rhs := make([]string, len(pk))
	for i, name := range pk {
		c := t.Col(name)
		if c == nil {
			return "", fmt.Errorf("%s.%s has no column %s", t.Schema, t.Name, name)
		}
		id := pgx.Identifier{name}.Sanitize()
		lhs[i] = "t." + id
		rhs[i] = fmt.Sprintf("(x ->> %s)::%s", quoteLiteral(name), c.Type)
	}
	return fmt.Sprintf(`SELECT to_jsonb(t) FROM %s t WHERE (%s) IN (SELECT %s FROM jsonb_array_elements($1::jsonb) AS x)`,
		t.Qualified(), strings.Join(lhs, ", "), strings.Join(rhs, ", ")), nil
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// resyncAll tells every change subscriber to refetch (after the LISTEN
// connection was lost).
func (h *rtHub) resyncAll() {
	h.mu.Lock()
	var chans []*rtChannel
	for ch := range h.channels {
		if len(ch.subs) > 0 {
			chans = append(chans, ch)
		}
	}
	h.mu.Unlock()
	for _, ch := range chans {
		ch.system("ok", evPostgresChanges, "resync")
	}
}

// closeAll ends every channel (the project's services went off).
func (h *rtHub) closeAll(reason string) {
	h.mu.Lock()
	var chans []*rtChannel
	for ch := range h.channels {
		chans = append(chans, ch)
	}
	h.mu.Unlock()
	for _, ch := range chans {
		ch.system("error", "", reason)
		ch.c.closeChannel(ch)
	}
}

// meterRealtime adds each project's connection time since the last count
// and its messages to the usage report.
func (e *Edge) meterRealtime() {
	e.rtMu.Lock()
	hubs := make([]*rtHub, 0, len(e.hubs))
	for _, h := range e.hubs {
		hubs = append(hubs, h)
	}
	e.rtMu.Unlock()
	now := time.Now()
	for _, h := range hubs {
		h.meterMu.Lock()
		elapsed := now.Sub(h.meteredAt)
		if h.meteredAt.IsZero() {
			elapsed = 0
		}
		h.meteredAt = now
		h.meterMu.Unlock()
		conns, msgs := h.conns.Load(), h.msgs.Swap(0)
		secs := max(conns, 0) * int64(elapsed.Round(time.Second)/time.Second)
		if secs <= 0 && msgs == 0 {
			continue
		}
		p := e.lookup(h.ref)
		if p == nil {
			continue
		}
		e.meter.realtime(p.cfg.ProjectID, secs, msgs)
	}
}

// dropCached drops the cached reads of the tables a batch of changes
// touched, on this edge (V4.1 §10): every edge listening to the project
// does the same, whatever wrote the rows.
func (h *rtHub) dropCached(p *project, batch []change) {
	if len(p.cfg.Settings.CacheTTLSeconds) == 0 {
		return
	}
	seen := map[string]bool{}
	var tables []string
	for _, c := range batch {
		if k := c.schema + "." + c.table; !seen[k] {
			seen[k] = true
			tables = append(tables, k)
		}
	}
	h.e.cache.dropTables(h.ref, tables)
}
