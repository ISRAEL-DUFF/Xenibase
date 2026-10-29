package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/datacat"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
)

// Realtime (V4 §6): one WebSocket per client at /realtime/v1/websocket,
// joining channels for database changes, broadcast and presence. Delivery
// is at most once to connected clients; a client that reconnects refetches.

const (
	// rtIdle closes a connection that sent nothing (not even a heartbeat,
	// sent every 25 seconds by the clients) for this long (§6.4).
	rtIdle = 2 * time.Minute
	// rtMaxChannels is the channels one connection may join.
	rtMaxChannels = 100
	// rtMaxFrame is the largest message a client may send.
	rtMaxFrame = 256 << 10
	// rtSendQueue is the messages queued to a slow client before it is
	// disconnected (it refetches on reconnect).
	rtSendQueue = 256
	// rtSendsPerMinute bounds a connection's broadcasts and presence
	// updates (each is fanned out, and relayed through the database).
	rtSendsPerMinute = 600
	// rtTopicPrefix starts every channel topic.
	rtTopicPrefix = "realtime:"
)

// rtConn is one client's WebSocket.
type rtConn struct {
	e     *Edge
	hub   *rtHub
	ws    *websocket.Conn
	v2    atomic.Bool // the client speaks the v2 wire format; set by the read loop, read by senders
	ip    string
	base  Request // the key's role: service for the secret key, else anon
	out   chan []byte
	done  chan struct{}
	close sync.Once

	mu       sync.Mutex
	channels map[string]*rtChannel // by topic

	lastSeen atomic.Int64 // unix nanoseconds
	started  time.Time
	id       string
}

// rtChannel is a channel a connection joined.
type rtChannel struct {
	c       *rtConn
	topic   string // "realtime:<name>"
	joinRef *string

	mu     sync.Mutex
	req    Request
	userID *uuid.UUID
	exp    time.Time // the user token's expiry (zero: none)

	private  bool
	bcSelf   bool
	bcAck    bool
	presence string // presence key
	// What a private channel's policies allow (public channels: all).
	readBroadcast, writeBroadcast, readPresence, writePresence bool

	subs []*rtSub
}

// rtSub is one postgres_changes binding of a channel.
type rtSub struct {
	id     int64
	ch     *rtChannel
	event  string // * INSERT UPDATE DELETE
	schema string
	table  string
	filter *rtFilter
	raw    string // the filter as given
	// seen are the rows (by primary key) the subscriber was sent, so a
	// delete reaches only those who could see the row (§6.3).
	seen *pkSet
}

var rtSubIDs atomic.Int64

// realtime serves /realtime/v1/... after the key check.
func (e *Edge) realtime(c *call, req Request) {
	switch c.r.URL.Path {
	case "/realtime/v1/websocket", "/realtime/v1/websocket/":
	case "/realtime/v1/api/broadcast":
		e.restBroadcast(c, req)
		return
	default:
		if topic, ok := strings.CutPrefix(c.r.URL.Path, "/realtime/v1/history/"); ok && c.r.Method == http.MethodGet {
			e.history(c, req, topic)
			return
		}
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
		return
	}
	rc := c.p.cfg.Realtime
	if rc.MessagesBlocked {
		c.fail(http.StatusTooManyRequests, "realtime_quota_exceeded", "the organisation's realtime messages for this month are used up")
		return
	}
	if c.p.cfg.SpendCapped {
		// Connections already open stay; new ones wait for the cap (V4 §12).
		c.fail(http.StatusTooManyRequests, "spend_cap_reached", "the organisation has reached its spend cap; new realtime connections are refused")
		return
	}
	hub := e.hub(c.p.cfg.Ref)
	if !hub.admit(rc.MaxConnections) {
		hub.release()
		c.fail(http.StatusTooManyRequests, "too_many_connections", "the project has as many realtime connections as its plan allows")
		return
	}
	defer hub.release()
	ws, err := websocket.Accept(c.w.ResponseWriter, c.r, &websocket.AcceptOptions{
		// The key check and allowed origins already ran (CORS); browsers
		// connect from the app's own origin.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept answered
	}
	c.w.status = http.StatusSwitchingProtocols
	ws.SetReadLimit(rtMaxFrame)
	base := req
	base.Claims = map[string]any{"role": req.Role}
	if req.Role == "user" {
		// A bearer token on the upgrade (server-side clients) is the
		// connection's default; channels may send their own.
		base.Claims = req.Claims
	}
	conn := &rtConn{e: e, hub: hub, ws: ws, ip: c.ip, base: base,
		out: make(chan []byte, rtSendQueue), done: make(chan struct{}), channels: map[string]*rtChannel{}, started: time.Now(), id: randRef()}
	conn.v2.Store(c.r.URL.Query().Get("vsn") == "2.0.0")
	conn.lastSeen.Store(time.Now().UnixNano())
	conn.run(c.r.Context())
}

// run serves the connection until either side closes it.
func (rc *rtConn) run(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	go rc.writeLoop(ctx)
	go rc.idleLoop(ctx)
	defer rc.shutdown(websocket.StatusNormalClosure, "")
	for {
		_, data, err := rc.ws.Read(ctx)
		if err != nil {
			return
		}
		rc.lastSeen.Store(time.Now().UnixNano())
		m, v2, err := decodeMsg(data)
		if err != nil {
			rc.shutdown(websocket.StatusUnsupportedData, "not a realtime message")
			return
		}
		if v2 {
			rc.v2.Store(true)
		}
		rc.handle(ctx, m)
	}
}

func (rc *rtConn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-rc.done:
			return
		case b := <-rc.out:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := rc.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				rc.shutdown(websocket.StatusGoingAway, "")
				return
			}
		}
	}
}

func (rc *rtConn) idleLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rc.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, rc.lastSeen.Load())) > rtIdle {
				rc.shutdown(websocket.StatusPolicyViolation, "no heartbeat")
				return
			}
		}
	}
}

// shutdown leaves every channel and closes the socket, once.
func (rc *rtConn) shutdown(code websocket.StatusCode, reason string) {
	rc.close.Do(func() {
		close(rc.done)
		rc.mu.Lock()
		chans := make([]*rtChannel, 0, len(rc.channels))
		for _, ch := range rc.channels {
			chans = append(chans, ch)
		}
		rc.channels = map[string]*rtChannel{}
		rc.mu.Unlock()
		for _, ch := range chans {
			rc.hub.leave(ch)
		}
		_ = rc.ws.Close(code, reason)
	})
}

// send queues m; a client too slow to keep up is disconnected.
func (rc *rtConn) send(m rtMsg) bool {
	b := encodeMsg(m, rc.v2.Load())
	select {
	case <-rc.done:
		return false
	default:
	}
	select {
	case rc.out <- b:
		if billable(m) {
			rc.hub.counted(1)
		}
		return true
	default:
		go rc.shutdown(websocket.StatusTryAgainLater, "too slow to keep up; reconnect and refetch")
		return false
	}
}

func (rc *rtConn) reply(m rtMsg, status string, response any) {
	if response == nil {
		response = map[string]any{}
	}
	rc.send(rtMsg{JoinRef: m.JoinRef, Ref: m.Ref, Topic: m.Topic, Event: phxReply,
		Payload: rawJSON(map[string]any{"status": status, "response": response})})
}

func (rc *rtConn) replyError(m rtMsg, reason string) {
	rc.reply(m, "error", map[string]any{"reason": reason})
}

// system sends a channel a system message (subscribed, errors, resync).
func (ch *rtChannel) system(status, extension, message string) {
	ch.c.send(rtMsg{JoinRef: ch.joinRef, Topic: ch.topic, Event: evSystem, Payload: rawJSON(map[string]any{
		"status": status, "extension": extension, "message": message, "channel": strings.TrimPrefix(ch.topic, rtTopicPrefix)})})
}

// billable reports whether a message counts as one of the month's realtime
// messages: what clients send each other and what the edge delivers, not
// the protocol around it (heartbeats, joins, leaves, tokens, replies and
// status messages).
func billable(m rtMsg) bool {
	if m.Topic == phxTopic {
		return false
	}
	switch m.Event {
	case evBroadcast, evPresence, evPresenceState, evPresenceDiff, evPostgresChanges:
		return true
	}
	return false
}

func (rc *rtConn) handle(ctx context.Context, m rtMsg) {
	if billable(m) {
		rc.hub.counted(1)
	}
	if m.Topic == phxTopic {
		if m.Event == phxHeartbeat {
			rc.reply(m, "ok", nil)
		}
		return
	}
	rc.mu.Lock()
	ch := rc.channels[m.Topic]
	rc.mu.Unlock()
	if m.Event == phxJoin {
		rc.join(ctx, m, ch)
		return
	}
	if ch == nil {
		rc.replyError(m, "join the channel first")
		return
	}
	switch m.Event {
	case phxLeave:
		rc.mu.Lock()
		delete(rc.channels, m.Topic)
		rc.mu.Unlock()
		rc.hub.leave(ch)
		rc.reply(m, "ok", nil)
		rc.send(rtMsg{JoinRef: ch.joinRef, Ref: m.Ref, Topic: m.Topic, Event: phxClose})
	case evAccessToken:
		var p struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(m.Payload, &p)
		if err := ch.setToken(rc, p.AccessToken); err != nil {
			ch.system("error", "", err.Error())
			rc.closeChannel(ch)
			return
		}
		rc.reply(m, "ok", nil)
	case evBroadcast, evPresence:
		if !rc.e.limits.allow("rt:"+rc.id, rtSendsPerMinute) {
			rc.replyError(m, fmt.Sprintf("rate_limited: at most %d broadcasts and presence updates a minute per connection", rtSendsPerMinute))
			return
		}
		if m.Event == evBroadcast {
			rc.hub.broadcastFrom(ctx, ch, m)
		} else {
			rc.hub.presenceFrom(ctx, ch, m)
		}
	default:
		rc.replyError(m, "unknown event "+m.Event)
	}
}

// closeChannel removes ch and tells the client.
func (rc *rtConn) closeChannel(ch *rtChannel) {
	rc.mu.Lock()
	if rc.channels[ch.topic] == ch {
		delete(rc.channels, ch.topic)
	}
	rc.mu.Unlock()
	rc.hub.leave(ch)
	rc.send(rtMsg{JoinRef: ch.joinRef, Topic: ch.topic, Event: phxClose})
}

// joinConfig is phx_join's payload (supabase-js's shape).
type joinConfig struct {
	Config struct {
		Broadcast struct {
			Ack  bool `json:"ack"`
			Self bool `json:"self"`
		} `json:"broadcast"`
		Presence struct {
			Key string `json:"key"`
		} `json:"presence"`
		PostgresChanges []struct {
			Event  string `json:"event"`
			Schema string `json:"schema"`
			Table  string `json:"table"`
			Filter string `json:"filter"`
		} `json:"postgres_changes"`
		Private bool `json:"private"`
	} `json:"config"`
	AccessToken string `json:"access_token"`
}

// setToken changes the channel's user from an access token: "" keeps the
// connection's key role.
func (ch *rtChannel) setToken(rc *rtConn, token string) error {
	req := rc.base
	var uid *uuid.UUID
	var exp time.Time
	if token = strings.TrimSpace(token); token != "" && rc.base.Role != "service" {
		p := rc.e.lookup(rc.hub.ref)
		if p == nil {
			return errors.New("the project's backend services are off")
		}
		// supabase-js sends the publishable key as the token when signed out.
		if _, isKey := p.keys[edgeapi.HashKey(token)]; !isKey {
			claims, err := jwtes.Verify(token, p.jwks, p.cfg.Ref, time.Now())
			if err != nil {
				return fmt.Errorf("invalid token: %w", err)
			}
			if claims["role"] != "user" {
				return errors.New("invalid token: not a user's access token")
			}
			req = Request{Role: "user", Claims: claims, Timeout: rc.base.Timeout}
			if mfaRequired(p.cfg.Auth, req) {
				return errors.New("this project requires a second factor: verify one to reach aal2")
			}
			if sub, _ := claims["sub"].(string); sub != "" {
				if u, err := uuid.Parse(sub); err == nil {
					uid = &u
				}
			}
			if f, ok := claims["exp"].(float64); ok {
				exp = time.Unix(int64(f), 0)
			}
		}
	}
	ch.mu.Lock()
	ch.req, ch.userID, ch.exp = req, uid, exp
	ch.mu.Unlock()
	return nil
}

// request is who the channel acts as now; expired is a user token past its
// expiry.
func (ch *rtChannel) request() (req Request, expired bool) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.req, !ch.exp.IsZero() && time.Now().After(ch.exp)
}

func (rc *rtConn) join(ctx context.Context, m rtMsg, existing *rtChannel) {
	if !strings.HasPrefix(m.Topic, rtTopicPrefix) || len(m.Topic) == len(rtTopicPrefix) || len(m.Topic) > 255 {
		rc.replyError(m, "a topic is realtime:<name>")
		return
	}
	if existing != nil {
		// Rejoining (a client reconnecting a channel) replaces it.
		rc.mu.Lock()
		delete(rc.channels, m.Topic)
		rc.mu.Unlock()
		rc.hub.leave(existing)
	}
	rc.mu.Lock()
	n := len(rc.channels)
	rc.mu.Unlock()
	if n >= rtMaxChannels {
		rc.replyError(m, fmt.Sprintf("a connection may join at most %d channels", rtMaxChannels))
		return
	}
	var jc joinConfig
	if len(m.Payload) > 0 {
		if err := json.Unmarshal(m.Payload, &jc); err != nil {
			rc.replyError(m, "the join payload isn't valid JSON")
			return
		}
	}
	ch := &rtChannel{c: rc, topic: m.Topic, joinRef: m.JoinRef, private: jc.Config.Private,
		bcSelf: jc.Config.Broadcast.Self, bcAck: jc.Config.Broadcast.Ack, presence: jc.Config.Presence.Key,
		readBroadcast: true, writeBroadcast: true, readPresence: true, writePresence: true}
	if ch.joinRef == nil {
		ch.joinRef = m.Ref
	}
	if ch.presence == "" {
		ch.presence = uuid.NewString()
	}
	if err := ch.setToken(rc, jc.AccessToken); err != nil {
		rc.replyError(m, err.Error())
		return
	}
	p := rc.e.lookup(rc.hub.ref)
	if p == nil {
		rc.replyError(m, "the project's backend services are off")
		return
	}
	req, _ := ch.request()
	jctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if ch.private {
		a, err := rc.e.channelAccess(jctx, p, req, ch.topic)
		if err == nil && !a.readBroadcast && !a.readPresence {
			err = errNotMember
		}
		if err != nil {
			rc.replyError(m, err.Error())
			return
		}
		ch.readBroadcast, ch.writeBroadcast, ch.readPresence, ch.writePresence = a.readBroadcast, a.writeBroadcast, a.readPresence, a.writePresence
	}
	var bound []map[string]any
	for _, pc := range jc.Config.PostgresChanges {
		sub, err := rc.e.bindChanges(jctx, p, req, ch, pc.Event, pc.Schema, pc.Table, pc.Filter)
		if err != nil {
			rc.replyError(m, err.Error())
			return
		}
		ch.subs = append(ch.subs, sub)
		bound = append(bound, map[string]any{"id": sub.id, "event": sub.event, "schema": sub.schema, "table": sub.table, "filter": sub.raw})
	}
	rc.mu.Lock()
	rc.channels[m.Topic] = ch
	rc.mu.Unlock()
	// The hub owns ch.subs once joined (it drops a subscription whose
	// table loses row-level security).
	changes := len(ch.subs) > 0
	rc.hub.join(ch)
	if changes {
		rc.hub.waitReady(jctx)
	}
	resp := map[string]any{}
	if len(bound) > 0 {
		resp["postgres_changes"] = bound
	}
	rc.reply(m, "ok", resp)
	if len(bound) > 0 {
		ch.system("ok", evPostgresChanges, "Subscribed to PostgreSQL")
	}
	if ch.readPresence {
		rc.hub.presenceState(ch)
	}
}

// bindChanges checks a postgres_changes binding: the table has realtime on,
// is in an exposed schema, the caller may read it, and (for anon and users)
// it has row-level security or is listed as public.
func (e *Edge) bindChanges(ctx context.Context, p *project, req Request, ch *rtChannel, event, schema, table, filter string) (*rtSub, error) {
	event = strings.ToUpper(strings.TrimSpace(event))
	switch event {
	case "", "*":
		event = "*"
	case "INSERT", "UPDATE", "DELETE":
	default:
		return nil, fmt.Errorf("event %q: use *, INSERT, UPDATE or DELETE", event)
	}
	if schema == "" {
		schema = "public"
	}
	if table == "" || table == "*" {
		return nil, errors.New("postgres_changes needs a table (every table at once isn't supported)")
	}
	f, err := parseFilter(filter)
	if err != nil {
		return nil, err
	}
	hub := e.hub(p.cfg.Ref)
	enabled, err := hub.enabledTables(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("realtime can't reach the project's database: %w", err)
	}
	if !enabled[schema+"."+table] {
		// Just turned on: read the list again rather than wait out the cache.
		hub.forgetTables()
		if enabled, err = hub.enabledTables(ctx, p); err != nil {
			return nil, fmt.Errorf("realtime can't reach the project's database: %w", err)
		}
	}
	if !enabled[schema+"."+table] {
		return nil, fmt.Errorf("realtime is off for %s.%s: turn it on in the dashboard or with xb_realtime.enable('%s.%s')", schema, table, schema, table)
	}
	var t *datacat.Table
	err = e.WithRequest(ctx, p, req, func(tx pgx.Tx) error {
		cat, _, err := e.catalog(ctx, p, tx)
		if err != nil {
			return err
		}
		t = cat.ByName[schema+"."+table]
		if t == nil {
			return nil
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1::regclass, 'SELECT')`, t.Qualified()).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			t = nil
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("realtime can't reach the project's database: %w", err)
	}
	if t == nil {
		return nil, fmt.Errorf("no table %s.%s in the API's exposed schemas that this key or user may read", schema, table)
	}
	if req.Role != "service" && !t.RLS && !p.publicTable(t) {
		return nil, fmt.Errorf("%s.%s has no row-level security: turn it on (or list the table as public) before anon or users subscribe", schema, table)
	}
	if f != nil && t.Col(f.Column) == nil {
		return nil, fmt.Errorf("filter: %s.%s has no column %q", schema, table, f.Column)
	}
	return &rtSub{id: rtSubIDs.Add(1), ch: ch, event: event, schema: schema, table: table, filter: f, raw: filter, seen: newPKSet(10000)}, nil
}

// rtAccess is what a private channel's policies allow (§6.1).
type rtAccess struct {
	readBroadcast, writeBroadcast, readPresence, writePresence bool
}

// errNotMember refuses a private channel whose policies let the user read
// nothing.
var errNotMember = errors.New("not allowed to join this private channel: no policy on xb_realtime.channel_access lets this user read it")

// channelAccess asks the owner's policies on xb_realtime.channel_access
// what req may do on a private topic (§6.1), in a transaction rolled
// back: probe() adds a row as the platform; seeing it means may receive,
// adding one may send. The secret key may do everything. An error is the
// policies not being reachable, not a refusal.
func (e *Edge) channelAccess(ctx context.Context, p *project, req Request, topic string) (rtAccess, error) {
	if req.Role == "service" {
		return rtAccess{true, true, true, true}, nil
	}
	name := strings.TrimPrefix(topic, rtTopicPrefix)
	var r [4]bool
	err := e.WithRequest(ctx, p, req, func(tx pgx.Tx) error {
		for i, ext := range []string{"broadcast", "presence"} {
			var id int64
			if err := tx.QueryRow(ctx, `SELECT xb_realtime.probe($1, $2)`, name, ext).Scan(&id); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM xb_realtime.channel_access WHERE id = $1)`, id).Scan(&r[i*2]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SAVEPOINT w`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO xb_realtime.channel_access (topic, extension) VALUES ($1, $2)`, name, ext)
			r[i*2+1] = err == nil
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT w`); err != nil {
				return err
			}
		}
		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		return rtAccess{}, fmt.Errorf("realtime can't check the channel's policies: %w", err)
	}
	return rtAccess{readBroadcast: r[0], writeBroadcast: r[1], readPresence: r[2], writePresence: r[3]}, nil
}

// errRollback ends a transaction without keeping what it did.
var errRollback = errors.New("rollback")

// pkSet is a bounded set of primary keys (oldest out first).
type pkSet struct {
	mu    sync.Mutex
	max   int
	set   map[string]struct{}
	order []string
}

func newPKSet(n int) *pkSet { return &pkSet{max: n, set: map[string]struct{}{}} }

func (s *pkSet) add(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[k]; ok {
		return
	}
	s.set[k] = struct{}{}
	s.order = append(s.order, k)
	for len(s.order) > s.max {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *pkSet) has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[k]
	return ok
}

func (s *pkSet) remove(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.set, k)
}

func randRef() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
