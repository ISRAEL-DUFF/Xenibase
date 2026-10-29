package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// Realtime's unit tests: a connection without a socket (testConn) whose
// queued messages the test reads, on a project without a database.

const rtTestRef = "rtunit01"

// rtTestEdge is an edge with one active project and its two keys.
func rtTestEdge(t *testing.T) (*Edge, edgeapi.Project) {
	t.Helper()
	pc := edgeapi.Project{Ref: rtTestRef, ProjectID: uuid.New(), OrgID: uuid.New(), State: edgeapi.StateActive, Version: 1,
		Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey("xb_pub_rtunit")},
			{ID: uuid.New(), Kind: edgeapi.KindSecret, Hash: edgeapi.HashKey("xb_sec_rtunit")}}}
	e := New(Config{Domain: "edge.test"})
	e.apply([]edgeapi.Project{pc})
	e.ready = true
	return e, pc
}

// testConn is a connection on h with no socket behind it, opened with the
// project's key for base's role.
func testConn(h *rtHub, base Request) *rtConn {
	if base.Claims == nil {
		base.Claims = map[string]any{"role": base.Role}
	}
	rc := &rtConn{e: h.e, hub: h, base: base, out: make(chan []byte, rtSendQueue), done: make(chan struct{}),
		channels: map[string]*rtChannel{}, started: time.Now(), id: randRef()}
	if p := h.e.lookup(h.ref); p != nil {
		for _, k := range p.keys {
			if (k.Kind == edgeapi.KindSecret) == (base.Role == "service") {
				rc.keyID = k.ID
			}
		}
	}
	h.opened(rc)
	return rc
}

// testChannel is a public channel on rc, joined to the hub without starting
// its listener.
func testChannel(rc *rtConn, topic string) *rtChannel {
	ch := &rtChannel{c: rc, topic: topic, presence: uuid.NewString(), req: rc.base,
		acc: rtAccess{readBroadcast: true, writeBroadcast: true, readPresence: true, writePresence: true}}
	rc.mu.Lock()
	rc.channels[topic] = ch
	rc.mu.Unlock()
	h := rc.hub
	h.mu.Lock()
	h.channels[ch] = struct{}{}
	if h.byTopic[topic] == nil {
		h.byTopic[topic] = map[*rtChannel]struct{}{}
	}
	h.byTopic[topic][ch] = struct{}{}
	for _, s := range ch.subs {
		k := s.schema + "." + s.table
		if h.subs[k] == nil {
			h.subs[k] = map[*rtSub]struct{}{}
		}
		h.subs[k][s] = struct{}{}
	}
	h.mu.Unlock()
	return ch
}

// received drains what was queued to rc.
func received(t *testing.T, rc *rtConn) []rtMsg {
	t.Helper()
	var out []rtMsg
	for {
		select {
		case b := <-rc.out:
			m, _, err := decodeMsg(b)
			if err != nil {
				t.Fatalf("queued message %s: %v", b, err)
			}
			out = append(out, m)
		default:
			return out
		}
	}
}

func msg(topic, event string, payload any) rtMsg {
	ref := randRef()
	b, _ := json.Marshal(payload)
	return rtMsg{Ref: &ref, Topic: topic, Event: event, Payload: b}
}

// TestRealtimeCountsOnlyMessages: heartbeats, their replies and other
// protocol replies aren't billed as realtime messages; broadcasts are,
// once from the sender and once to each receiver.
func TestRealtimeCountsOnlyMessages(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	ctx := context.Background()
	a, b := testConn(h, Request{Role: "anon"}), testConn(h, Request{Role: "anon"})
	testChannel(a, "realtime:room")
	testChannel(b, "realtime:room")

	for range 10 {
		a.handle(ctx, msg(phxTopic, phxHeartbeat, map[string]any{}))
	}
	a.handle(ctx, msg("realtime:room", phxLeave, map[string]any{}))
	if got := received(t, a); len(got) != 12 { // ten heartbeat replies, the leave's reply and phx_close
		t.Fatalf("replies: %d %+v", len(got), got)
	}
	if n := h.msgs.Load(); n != 0 {
		t.Fatalf("heartbeats and a leave counted as %d messages", n)
	}
	b.handle(ctx, msg("realtime:room", evBroadcast, map[string]any{"type": "broadcast", "event": "ping", "payload": map[string]any{}}))
	if n := h.msgs.Load(); n != 1 { // b's broadcast; nobody else is on the topic now
		t.Fatalf("a broadcast counted as %d messages", n)
	}
	testChannel(a, "realtime:room")
	b.handle(ctx, msg("realtime:room", evBroadcast, map[string]any{"type": "broadcast", "event": "ping", "payload": map[string]any{}}))
	if n := h.msgs.Load(); n != 3 { // sent, and received by a
		t.Fatalf("a broadcast to one other client counted as %d messages in all, want 3", n)
	}
}

// replies are rc's queued replies' payloads, and broadcasts the number of
// broadcasts queued to it.
func replies(t *testing.T, rc *rtConn) (out []string, broadcasts int) {
	t.Helper()
	for _, m := range received(t, rc) {
		switch m.Event {
		case phxReply:
			out = append(out, string(m.Payload))
		case evBroadcast:
			broadcasts++
		}
	}
	return out, broadcasts
}

func broadcastOf(n int) map[string]any {
	return map[string]any{"type": "broadcast", "event": "blob", "payload": map[string]any{"data": strings.Repeat("x", n)}}
}

// TestRealtimeBroadcastLimits: a broadcast or presence update over 32 KB
// is refused, and so is one past the project's 1 MB a second on this
// process, however many connections send them.
func TestRealtimeBroadcastLimits(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	ctx := context.Background()
	to := testConn(h, Request{Role: "anon"})
	testChannel(to, "realtime:room")

	from := testConn(h, Request{Role: "anon"})
	testChannel(from, "realtime:room")
	from.handle(ctx, msg("realtime:room", evBroadcast, broadcastOf(rtMaxMessage)))
	from.handle(ctx, msg("realtime:room", evPresence, map[string]any{"type": "presence", "event": "track", "payload": map[string]any{"data": strings.Repeat("x", rtMaxMessage)}}))
	got, _ := replies(t, from)
	if len(got) != 2 || !strings.Contains(got[0], "too_large") || !strings.Contains(got[1], "too_large") {
		t.Fatalf("a message over %d bytes: %v", rtMaxMessage, got)
	}
	if _, n := replies(t, to); n != 0 {
		t.Fatalf("an oversized broadcast was delivered %d times", n)
	}

	// 30 KB broadcasts from several connections: about 34 fit in a second.
	sent, refused := 0, 0
	for i := 0; i < 50; i++ {
		rc := testConn(h, Request{Role: "anon"})
		testChannel(rc, "realtime:room")
		rc.handle(ctx, msg("realtime:room", evBroadcast, broadcastOf(30<<10)))
		r, _ := replies(t, rc)
		if len(r) == 1 && strings.Contains(r[0], "rate_limited") {
			refused++
		} else {
			sent++
		}
		h.leave(rc.channels["realtime:room"])
	}
	if _, n := replies(t, to); n != sent || sent > rtBytesPerSecond/(30<<10) || refused == 0 {
		t.Fatalf("sent %d, refused %d, delivered %d", sent, refused, n)
	}
}

// TestRealtimeRelayParts: a message too large for one NOTIFY goes to the
// other processes in parts that each fit, joined back in any order;
// incomplete ones are dropped after a while.
func TestRealtimeRelayParts(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	ctx := context.Background()
	to := testConn(h, Request{Role: "anon"})
	testChannel(to, "realtime:room")

	payload, _ := json.Marshal(broadcastOf(30 << 10))
	b, _ := json.Marshal(relayMsg{Node: "edge-other-1", Kind: "bc", Topic: "realtime:room", Payload: payload})
	parts := splitRelay("edge-other-1", 7, b)
	if len(parts) < 5 {
		t.Fatalf("%d bytes in %d parts", len(b), len(parts))
	}
	for _, p := range parts {
		if len(p) > rtNotifyMax {
			t.Fatalf("a part of %d bytes", len(p))
		}
	}
	if one := splitRelay("edge-other-1", 8, []byte(`{"k":"hi"}`)); len(one) != 1 || one[0] != `{"k":"hi"}` {
		t.Fatalf("a small message: %v", one)
	}
	// Last first, and one twice.
	for _, i := range []int{len(parts) - 1, 0, 0} {
		h.relayIn(ctx, nil, parts[i])
	}
	if _, n := replies(t, to); n != 0 {
		t.Fatal("delivered before every part was in")
	}
	for _, p := range parts[1 : len(parts)-1] {
		h.relayIn(ctx, nil, p)
	}
	got := received(t, to)
	if len(got) != 1 || got[0].Event != evBroadcast || string(got[0].Payload) != string(payload) {
		t.Fatalf("joined: %d messages", len(got))
	}
	if len(h.bc.parts) != 0 || h.bc.partBytes != 0 {
		t.Fatalf("parts kept: %d, %d bytes", len(h.bc.parts), h.bc.partBytes)
	}

	// A message whose last part never comes is let go.
	parts = splitRelay("edge-other-1", 9, b)
	for _, p := range parts[:len(parts)-1] {
		h.relayIn(ctx, nil, p)
	}
	for _, ps := range h.bc.parts {
		ps.at = time.Now().Add(-rtPartsWait - time.Second)
	}
	h.relayIn(ctx, nil, `{"n":"edge-other-1","k":"hi"}`)
	h.relayIn(ctx, nil, splitRelay("edge-other-1", 10, b)[0])
	if len(h.bc.parts) != 1 || h.bc.partBytes > len(b) {
		t.Fatalf("after expiry: %d messages, %d bytes", len(h.bc.parts), h.bc.partBytes)
	}
}

// rtServe is the test edge behind an HTTP server.
func rtServe(t *testing.T, e *Edge) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

// rtDial opens a realtime WebSocket to the test project with key.
func rtDial(t *testing.T, srv *httptest.Server, ref, key string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(srv.URL, "http://", "ws://", 1) + "/realtime/v1/websocket?vsn=1.0.0&apikey=" + url.QueryEscape(key)
	ws, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{Host: ref + ".edge.test"})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// TestRealtimeFrameLimit: a frame over 64 KB closes the connection.
func TestRealtimeFrameLimit(t *testing.T) {
	e, _ := rtTestEdge(t)
	ws := rtDial(t, rtServe(t, e), rtTestRef, "xb_pub_rtunit")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	big, _ := json.Marshal(map[string]any{"topic": "realtime:room", "event": evBroadcast, "ref": "1",
		"payload": broadcastOf(rtMaxFrame)})
	_ = ws.Write(ctx, websocket.MessageText, big)
	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("a frame of %d bytes: %v", len(big), err)
	}
}

// TestRealtimeSubscriptionCaps: a join may bind at most 20 subscriptions,
// and a connection hold at most 100, refused before any is checked.
func TestRealtimeSubscriptionCaps(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	ctx := context.Background()
	rc := testConn(h, Request{Role: "anon"})
	changes := func(n int) map[string]any {
		pcs := make([]map[string]any, n)
		for i := range pcs {
			pcs[i] = map[string]any{"event": "*", "schema": "public", "table": "items"}
		}
		return map[string]any{"config": map[string]any{"postgres_changes": pcs}}
	}
	rc.join(ctx, msg("realtime:many", phxJoin, changes(rtMaxSubs+1)), nil)
	if got, _ := replies(t, rc); len(got) != 1 || !strings.Contains(got[0], "a channel may have at most 20") {
		t.Fatalf("a join with %d subscriptions: %v", rtMaxSubs+1, got)
	}
	// 95 subscriptions already, over five channels.
	for i := range 5 {
		ch := testChannel(rc, fmt.Sprintf("realtime:held%d", i))
		for range 19 {
			ch.subs = append(ch.subs, &rtSub{ch: ch, schema: "public", table: "items", seen: newPKSet(rtSeenKeys, &rc.seen, rtConnSeenKeys)})
		}
	}
	rc.join(ctx, msg("realtime:more", phxJoin, changes(6)), nil)
	if got, _ := replies(t, rc); len(got) != 1 || !strings.Contains(got[0], "a connection may have at most 100") {
		t.Fatalf("a join past %d subscriptions on the connection: %v", rtMaxConnSubs, got)
	}
}

// TestRealtimeSeenKeysBounded: a connection's subscriptions remember at
// most rtConnSeenKeys keys between them, each its own newest; a key
// removed and added again counts from when it came back.
func TestRealtimeSeenKeysBounded(t *testing.T) {
	var shared atomic.Int64
	a, b := newPKSet(10, &shared, 15), newPKSet(10, &shared, 15)
	for i := range 10 {
		a.add(fmt.Sprint("a", i))
		b.add(fmt.Sprint("b", i))
	}
	if n := shared.Load(); n != 15 || len(a.set)+len(b.set) != 15 || len(a.set) < 5 {
		t.Fatalf("shared %d, a %d, b %d", n, len(a.set), len(b.set))
	}
	if !b.has("b9") || b.has("b0") {
		t.Fatal("b let its newest go")
	}
	b.release()
	if n := shared.Load(); n != int64(len(a.set)) {
		t.Fatalf("after b ended: shared %d, a %d", n, len(a.set))
	}

	s := newPKSet(3, nil, 0)
	for _, k := range []string{"x", "y", "z"} {
		s.add(k)
	}
	s.remove("x")
	s.add("x")
	s.add("w") // y goes, not the x that came back
	if !s.has("x") || s.has("y") || !s.has("z") || !s.has("w") {
		t.Fatalf("set %v", s.set)
	}
	for i := range 1000 {
		s.add(fmt.Sprint(i))
		s.remove(fmt.Sprint(i))
	}
	if len(s.order) > 2*len(s.set)+64 {
		t.Fatalf("%d entries kept for %d keys", len(s.order), len(s.set))
	}
}

// TestRealtimeSendQueueBytes: a client that stops reading is disconnected
// once 4 MB wait for it, though a single larger message still goes.
func TestRealtimeSendQueueBytes(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	rc := testConn(h, Request{Role: "anon"})
	huge := rtMsg{Topic: "realtime:room", Event: evBroadcast, Payload: json.RawMessage(`"` + strings.Repeat("x", 5<<20) + `"`)}
	if !rc.send(huge) {
		t.Fatal("one message larger than the queue's bound, on an empty queue, was refused")
	}
	<-rc.out
	rc.queued.Add(-int64(len(encodeMsg(huge, false))))
	mb := rtMsg{Topic: "realtime:room", Event: evBroadcast, Payload: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}
	for i := range 3 {
		if !rc.send(mb) {
			t.Fatalf("message %d refused", i+1)
		}
	}
	if rc.send(mb) {
		t.Fatal("a fourth MB queued to a client that reads nothing")
	}
	select {
	case <-rc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow client wasn't disconnected")
	}
}

// closedFor waits for rc to be told channel topic is closed for reason.
func closedFor(t *testing.T, rc *rtConn, topic, reason string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	told := false
	for {
		select {
		case b := <-rc.out:
			m, _, _ := decodeMsg(b)
			if m.Topic != topic {
				continue
			}
			switch {
			case m.Event == evSystem && strings.Contains(string(m.Payload), reason):
				told = true
			case m.Event == phxClose && told:
				rc.mu.Lock()
				_, still := rc.channels[topic]
				rc.mu.Unlock()
				if still {
					t.Fatalf("%s: closed but still joined", topic)
				}
				return
			case m.Event == evBroadcast || m.Event == evPresenceDiff:
				t.Fatalf("%s: %s delivered to a channel that should be closed", topic, m.Event)
			}
		case <-deadline:
			t.Fatalf("%s wasn't closed for %q", topic, reason)
		}
	}
}

// TestRealtimeTokenExpiry: a channel whose user's token has expired gets
// no broadcast or presence and is closed, by the message or by the timer;
// one on a connection upgraded with a bearer token expires with that
// token, and can't be joined once it has.
func TestRealtimeTokenExpiry(t *testing.T) {
	e, _ := rtTestEdge(t)
	h := e.hub(rtTestRef)
	ctx := context.Background()
	sender := testConn(h, Request{Role: "anon"})
	testChannel(sender, "realtime:room")
	user := func(exp time.Time) *rtConn {
		return testConn(h, Request{Role: "user", Claims: map[string]any{"role": "user", "sub": uuid.NewString(), "exp": float64(exp.Unix())}})
	}

	// A bearer-upgraded connection's channel takes its token's expiry.
	bearer := user(time.Now().Add(time.Hour))
	ch := testChannel(bearer, "realtime:room")
	if err := ch.setToken(bearer, ""); err != nil {
		t.Fatal(err)
	}
	if _, expired := ch.request(); expired || ch.exp.IsZero() || ch.userID == nil {
		t.Fatalf("a channel on a bearer connection: exp %v, user %v", ch.exp, ch.userID)
	}
	ch.mu.Lock()
	ch.exp = time.Now().Add(-time.Second)
	ch.mu.Unlock()
	sender.handle(ctx, msg("realtime:room", evBroadcast, broadcastOf(10)))
	closedFor(t, bearer, "realtime:room", "has expired")

	// Presence, the same.
	stale := user(time.Now().Add(-time.Minute))
	ch = testChannel(stale, "realtime:room")
	_ = ch.setToken(stale, "")
	sender.handle(ctx, msg("realtime:room", evPresence, map[string]any{"type": "presence", "event": "track", "payload": map[string]any{}}))
	closedFor(t, stale, "realtime:room", "has expired")

	// The timer, with nothing sent on the topic.
	quiet := user(time.Now().Add(-time.Minute))
	ch = testChannel(quiet, "realtime:lonely")
	_ = ch.setToken(quiet, "")
	quiet.recheck(ctx)
	closedFor(t, quiet, "realtime:lonely", "has expired")

	// And no new channel on it.
	quiet.join(ctx, msg("realtime:again", phxJoin, map[string]any{"config": map[string]any{}}), nil)
	if got, _ := replies(t, quiet); len(got) != 1 || !strings.Contains(got[0], "has expired") {
		t.Fatalf("a join on an expired bearer connection: %v", got)
	}
}

// closedWith waits for the edge to close ws, and returns its close code
// and reason.
func closedWith(t *testing.T, ws *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("not closed by the edge: %v", err)
			}
			return ce.Code, ce.Reason
		}
	}
}

// alive checks that ws still answers a heartbeat.
func alive(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"topic":"phoenix","event":"heartbeat","payload":{},"ref":"hb"}`)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, b, err := ws.Read(ctx); err != nil || !strings.Contains(string(b), `"hb"`) {
		t.Fatalf("heartbeat reply: %s %v", b, err)
	}
}

// TestRealtimeConnectionsFollowTheFeed: an open connection is closed when
// the key it was opened with is revoked, when the organisation is
// suspended, and when the project leaves the feed.
func TestRealtimeConnectionsFollowTheFeed(t *testing.T) {
	e, pc := rtTestEdge(t)
	srv := rtServe(t, e)
	h := e.hub(rtTestRef)
	pub, sec := rtDial(t, srv, rtTestRef, "xb_pub_rtunit"), rtDial(t, srv, rtTestRef, "xb_sec_rtunit")
	alive(t, pub)
	alive(t, sec)
	update := func(change func(*edgeapi.Project)) {
		pc.Version++
		change(&pc)
		e.apply([]edgeapi.Project{pc})
	}

	// The publishable key is rotated: its connection goes, the other stays.
	keys := pc.Keys
	update(func(p *edgeapi.Project) { p.Keys = keys[1:] })
	if code, reason := closedWith(t, pub); code != websocket.StatusPolicyViolation || !strings.Contains(reason, "revoked") {
		t.Fatalf("a revoked key's connection: %d %q", code, reason)
	}
	alive(t, sec)

	// The organisation is suspended.
	update(func(p *edgeapi.Project) { p.State = edgeapi.StateSuspended })
	if code, reason := closedWith(t, sec); code != websocket.StatusPolicyViolation || !strings.Contains(reason, "suspended") {
		t.Fatalf("a suspended project's connection: %d %q", code, reason)
	}

	// Services turned off: the project leaves the feed.
	update(func(p *edgeapi.Project) { p.State, p.Keys = edgeapi.StateActive, keys })
	ws := rtDial(t, srv, rtTestRef, "xb_pub_rtunit")
	alive(t, ws)
	update(func(p *edgeapi.Project) { p.State = edgeapi.StateDisabled })
	if code, reason := closedWith(t, ws); code != websocket.StatusPolicyViolation || !strings.Contains(reason, "off") {
		t.Fatalf("a connection to a project that left the feed: %d %q", code, reason)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.conns)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections still open on the hub", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRealtimeMonthlyLimitsCloseConnections: once the organisation has
// used its month's realtime messages, or its API requests, open
// connections are closed and HTTP broadcasts refused; a spend cap leaves
// them open.
func TestRealtimeMonthlyLimitsCloseConnections(t *testing.T) {
	e, pc := rtTestEdge(t)
	srv := rtServe(t, e)
	update := func(change func(*edgeapi.Project)) {
		pc.Version++
		change(&pc)
		e.apply([]edgeapi.Project{pc})
	}
	ws := rtDial(t, srv, rtTestRef, "xb_pub_rtunit")
	update(func(p *edgeapi.Project) { p.SpendCapped = true })
	time.Sleep(100 * time.Millisecond)
	alive(t, ws)

	update(func(p *edgeapi.Project) { p.Realtime.MessagesBlocked = true })
	if code, reason := closedWith(t, ws); code != websocket.StatusPolicyViolation || !strings.Contains(reason, "used up") {
		t.Fatalf("messages used up: %d %q", code, reason)
	}
	r := httptest.NewRequest(http.MethodPost, "http://edge/realtime/v1/api/broadcast",
		strings.NewReader(`{"messages":[{"topic":"room","event":"x","payload":{}}]}`))
	r.Host = rtTestRef + ".edge.test"
	r.Header.Set("apikey", "xb_sec_rtunit")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "realtime_quota_exceeded") {
		t.Fatalf("an HTTP broadcast with the messages used up: %d %s", w.Code, w.Body)
	}

	update(func(p *edgeapi.Project) { p.Realtime.MessagesBlocked, p.SpendCapped = false, false })
	ws = rtDial(t, srv, rtTestRef, "xb_pub_rtunit")
	alive(t, ws)
	update(func(p *edgeapi.Project) { p.RequestsBlocked = true })
	if code, reason := closedWith(t, ws); code != websocket.StatusPolicyViolation || !strings.Contains(reason, "API requests") {
		t.Fatalf("API requests used up: %d %q", code, reason)
	}
}
