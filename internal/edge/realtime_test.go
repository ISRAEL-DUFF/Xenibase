package edge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// testConn is a connection on h with no socket behind it.
func testConn(h *rtHub, base Request) *rtConn {
	if base.Claims == nil {
		base.Claims = map[string]any{"role": base.Role}
	}
	return &rtConn{e: h.e, hub: h, base: base, out: make(chan []byte, rtSendQueue), done: make(chan struct{}),
		channels: map[string]*rtChannel{}, started: time.Now(), id: randRef()}
}

// testChannel is a public channel on rc, joined to the hub without starting
// its listener.
func testChannel(rc *rtConn, topic string) *rtChannel {
	ch := &rtChannel{c: rc, topic: topic, presence: uuid.NewString(), req: rc.base,
		readBroadcast: true, writeBroadcast: true, readPresence: true, writePresence: true}
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
