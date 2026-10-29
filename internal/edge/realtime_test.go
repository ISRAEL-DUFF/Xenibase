package edge

import (
	"context"
	"encoding/json"
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
