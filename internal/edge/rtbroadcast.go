package edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Broadcast and presence (V4 §6.1). Clients on a topic reach each other
// through this process directly and through the project's other edge
// processes with NOTIFY on the project's database (§6.5), sent only while
// another process has said hello on it. Larger messages than a NOTIFY
// takes are sent in parts in one transaction, which writes nothing to a
// table; only one too large even for that (a big presence state) goes
// through xb_realtime.relay.

const (
	// rtNotifyMax is the largest NOTIFY payload sent as is (Postgres takes
	// 8000 bytes).
	rtNotifyMax = 7800
	// rtMaxParts is the most NOTIFY parts one message is sent in (about
	// 5 MB); rtPartsPending bounds the parts a process holds while it
	// waits for the rest, and rtPartsWait how long it waits.
	rtMaxParts     = 1000
	rtPartsPending = 16 << 20
	rtPartsWait    = 10 * time.Second
	// rtPersistTTL is how long the list of topics kept as history is cached.
	rtPersistTTL = 10 * time.Second

	// rtMaxMessage is the largest broadcast or presence message a client
	// sends, and the largest broadcast sent over HTTP.
	rtMaxMessage = 32 << 10
	// rtBytesPerSecond bounds the broadcasts and presence a project's
	// clients send through one edge process, all of which may be relayed
	// to its other processes through its database.
	rtBytesPerSecond = 1 << 20
	// rtHistoryBytesPerMinute bounds the broadcasts one edge process keeps
	// as a project's history.
	rtHistoryBytesPerMinute = 256 << 10
)

type rtBroadcastState struct {
	listening atomic.Bool

	mu    sync.Mutex
	peers map[string]time.Time // other processes' ids -> last heard
	// presence: topic key -> presence key -> phx_ref -> entry.
	presence map[string]map[string]map[string]*presenceEntry

	persistMu sync.Mutex
	persisted map[string]bool
	persistAt time.Time

	// What the project's clients sent, and what was kept as history,
	// lately.
	sent, kept byteRate

	// parts are messages from other processes still arriving in parts
	// (listener goroutine only), by node and id; partBytes their size.
	parts     map[string]*relayParts
	partBytes int
	partID    atomic.Int64
}

func (s *rtBroadcastState) init() {
	s.peers = map[string]time.Time{}
	s.presence = map[string]map[string]map[string]*presenceEntry{}
	s.parts = map[string]*relayParts{}
}

// byteRate counts bytes in a fixed window.
type byteRate struct {
	mu    sync.Mutex
	start time.Time
	used  int
}

// take adds n to the window's count; false (and nothing added) when that
// would pass limit bytes in the window.
func (r *byteRate) take(n, limit int, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.start) >= window {
		r.start, r.used = now, 0
	}
	if r.used+n > limit {
		return false
	}
	r.used += n
	return true
}

// relayParts is a message arriving in parts.
type relayParts struct {
	at   time.Time
	data [][]byte
	got  int
	size int
}

// presenceEntry is one tracked presence: on this process (ch set) or
// another (node).
type presenceEntry struct {
	node string
	ch   *rtChannel
	key  string
	meta map[string]any // includes phx_ref
}

// relayMsg is what processes tell each other.
type relayMsg struct {
	Node    string                                 `json:"n"`
	Kind    string                                 `json:"k"` // bc, pd, pst, hi, bye, ref, part
	Topic   string                                 `json:"t,omitempty"`
	Private bool                                   `json:"priv,omitempty"`
	Payload json.RawMessage                        `json:"p,omitempty"`
	Joins   map[string][]map[string]any            `json:"j,omitempty"`
	Leaves  map[string][]map[string]any            `json:"l,omitempty"`
	State   map[string]map[string][]map[string]any `json:"s,omitempty"` // topic key -> presence key -> metas
	First   bool                                   `json:"f,omitempty"`
	Relay   int64                                  `json:"r,omitempty"`
	// A part: the Part'th of Parts of message Relay, in base64.
	Part  int    `json:"pi,omitempty"`
	Parts int    `json:"pn,omitempty"`
	Data  string `json:"d,omitempty"`
}

// node is this process's id among the project's edge processes.
func (e *Edge) node() string {
	e.nodeOnce.Do(func() { e.nodeID = e.cfg.Name + "-" + randRef() })
	return e.nodeID
}

func topicKey(topic string, private bool) string {
	if private {
		return "p:" + topic
	}
	return "o:" + topic
}

// local is the channels on a topic with the same privacy.
func (h *rtHub) local(topic string, private bool) []*rtChannel {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*rtChannel
	for ch := range h.byTopic[topic] {
		if ch.private == private {
			out = append(out, ch)
		}
	}
	return out
}

// broadcastFrom sends a client's broadcast to the topic's other clients.
func (h *rtHub) broadcastFrom(ctx context.Context, ch *rtChannel, m rtMsg) {
	var body struct {
		Type    string          `json:"type"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(m.Payload, &body) != nil || body.Event == "" {
		ch.c.replyError(m, "a broadcast is {type: broadcast, event, payload}")
		return
	}
	if ch.private && !ch.writeBroadcast {
		ch.c.replyError(m, "not allowed to send on this private channel: no INSERT policy on xb_realtime.channel_access lets this user")
		return
	}
	if reason := h.budget(ctx, ch.topic, len(m.Payload)); reason != "" {
		ch.c.replyError(m, reason)
		return
	}
	h.fanOut(ch.topic, ch.private, m.Payload, ch)
	if ch.bcAck {
		ch.c.reply(m, "ok", nil)
	}
	h.relayOut(ctx, relayMsg{Kind: "bc", Topic: ch.topic, Private: ch.private, Payload: m.Payload})
	h.persist(ctx, ch.topic, ch.private, body.Event, body.Payload, ch)
}

// budget takes a message of n bytes from the project's byte rates on this
// process, or says why it can't be sent: it is too large, the project's
// clients are sending too much, or (a broadcast on a persisted topic) too
// much is being kept as history. topic is "" for presence.
func (h *rtHub) budget(ctx context.Context, topic string, n int) string {
	if n > rtMaxMessage {
		return fmt.Sprintf("too_large: a broadcast or presence message is at most %d KB", rtMaxMessage>>10)
	}
	if !h.bc.sent.take(n, rtBytesPerSecond, time.Second) {
		return fmt.Sprintf("rate_limited: the project's clients are sending more than %d MB of broadcasts and presence a second", rtBytesPerSecond>>20)
	}
	if topic == "" {
		return ""
	}
	if p := h.e.lookup(h.ref); p != nil && h.persisted(ctx, p)[strings.TrimPrefix(topic, rtTopicPrefix)] &&
		!h.bc.kept.take(n, rtHistoryBytesPerMinute, time.Minute) {
		return fmt.Sprintf("rate_limited: the project keeps at most %d KB of broadcast history a minute", rtHistoryBytesPerMinute>>10)
	}
	return ""
}

// fanOut delivers a broadcast payload to this process's clients on topic;
// from is the sending channel (nil from elsewhere).
func (h *rtHub) fanOut(topic string, private bool, payload json.RawMessage, from *rtChannel) {
	for _, to := range h.local(topic, private) {
		if to == from && !from.bcSelf {
			continue
		}
		if private && !to.readBroadcast {
			continue
		}
		to.c.send(rtMsg{JoinRef: to.joinRef, Topic: topic, Event: evBroadcast, Payload: payload})
	}
}

// persist keeps a broadcast as history when the owner listed its topic,
// with whether it was sent on the topic's private channel.
func (h *rtHub) persist(ctx context.Context, topic string, private bool, event string, payload json.RawMessage, from *rtChannel) {
	p := h.e.lookup(h.ref)
	if p == nil {
		return
	}
	name := strings.TrimPrefix(topic, rtTopicPrefix)
	if !h.persisted(ctx, p)[name] {
		return
	}
	var sender *uuid.UUID
	if from != nil {
		from.mu.Lock()
		sender = from.userID
		from.mu.Unlock()
	}
	pool, err := h.e.dbPool(ctx, p)
	if err != nil {
		return
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO xb_realtime.broadcast_history (topic, private, event, payload, sender) VALUES ($1, $2, $3, $4, $5)`,
		name, private, event, string(payload), sender); err != nil {
		h.e.cfg.Log.Warn("realtime history", "ref", h.ref, "err", err)
	}
}

func (h *rtHub) persisted(ctx context.Context, p *project) map[string]bool {
	s := &h.bc
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.persisted != nil && time.Since(s.persistAt) < rtPersistTTL {
		return s.persisted
	}
	out := map[string]bool{}
	if pool, err := h.e.dbPool(ctx, p); err == nil {
		if rows, err := pool.Query(ctx, `SELECT topic FROM xb_realtime.persisted_topics`); err == nil {
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil {
					out[t] = true
				}
			}
			rows.Close()
		}
	}
	s.persisted, s.persistAt = out, time.Now()
	return out
}

// presenceFrom tracks or untracks a client's presence.
func (h *rtHub) presenceFrom(ctx context.Context, ch *rtChannel, m rtMsg) {
	var body struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	if json.Unmarshal(m.Payload, &body) != nil {
		ch.c.replyError(m, "presence is {type: presence, event: track|untrack, payload}")
		return
	}
	if ch.private && !ch.writePresence {
		ch.c.replyError(m, "not allowed to track presence on this private channel")
		return
	}
	if reason := h.budget(ctx, "", len(m.Payload)); reason != "" {
		ch.c.replyError(m, reason)
		return
	}
	switch body.Event {
	case "track":
		meta := map[string]any{}
		for k, v := range body.Payload {
			meta[k] = v
		}
		meta["phx_ref"] = randRef()
		leaves := h.dropChannel(ch)
		e := &presenceEntry{node: h.e.node(), ch: ch, key: ch.presence, meta: meta}
		tk := topicKey(ch.topic, ch.private)
		h.bc.mu.Lock()
		if h.bc.presence[tk] == nil {
			h.bc.presence[tk] = map[string]map[string]*presenceEntry{}
		}
		if h.bc.presence[tk][e.key] == nil {
			h.bc.presence[tk][e.key] = map[string]*presenceEntry{}
		}
		h.bc.presence[tk][e.key][meta["phx_ref"].(string)] = e
		h.bc.mu.Unlock()
		joins := map[string][]map[string]any{e.key: {meta}}
		h.presenceDiff(ch.topic, ch.private, joins, leaves)
		h.relayOut(ctx, relayMsg{Kind: "pd", Topic: ch.topic, Private: ch.private, Joins: joins, Leaves: leaves})
	case "untrack":
		leaves := h.dropChannel(ch)
		if len(leaves) > 0 {
			h.presenceDiff(ch.topic, ch.private, nil, leaves)
			h.relayOut(ctx, relayMsg{Kind: "pd", Topic: ch.topic, Private: ch.private, Leaves: leaves})
		}
	default:
		ch.c.replyError(m, "presence events are track and untrack")
		return
	}
	ch.c.reply(m, "ok", nil)
}

// untrackChannel removes a leaving channel's presence.
func (h *rtHub) untrackChannel(ch *rtChannel) {
	leaves := h.dropChannel(ch)
	if len(leaves) == 0 {
		return
	}
	h.presenceDiff(ch.topic, ch.private, nil, leaves)
	h.relayOut(context.Background(), relayMsg{Kind: "pd", Topic: ch.topic, Private: ch.private, Leaves: leaves})
}

// dropChannel removes ch's entries and returns them as leaves.
func (h *rtHub) dropChannel(ch *rtChannel) map[string][]map[string]any {
	tk := topicKey(ch.topic, ch.private)
	h.bc.mu.Lock()
	defer h.bc.mu.Unlock()
	keys := h.bc.presence[tk]
	var leaves map[string][]map[string]any
	for key, refs := range keys {
		for ref, e := range refs {
			if e.ch != ch {
				continue
			}
			if leaves == nil {
				leaves = map[string][]map[string]any{}
			}
			leaves[key] = append(leaves[key], e.meta)
			delete(refs, ref)
		}
		if len(refs) == 0 {
			delete(keys, key)
		}
	}
	if len(keys) == 0 {
		delete(h.bc.presence, tk)
	}
	return leaves
}

// presenceDiff tells this process's clients on topic who came and went.
func (h *rtHub) presenceDiff(topic string, private bool, joins, leaves map[string][]map[string]any) {
	payload := rawJSON(map[string]any{"joins": metas(joins), "leaves": metas(leaves)})
	for _, to := range h.local(topic, private) {
		if private && !to.readPresence {
			continue
		}
		to.c.send(rtMsg{JoinRef: to.joinRef, Topic: topic, Event: evPresenceDiff, Payload: payload})
	}
}

func metas(m map[string][]map[string]any) map[string]any {
	out := map[string]any{}
	for k, ms := range m {
		out[k] = map[string]any{"metas": ms}
	}
	return out
}

// presenceState sends a joining channel everyone present on its topic.
func (h *rtHub) presenceState(ch *rtChannel) {
	tk := topicKey(ch.topic, ch.private)
	state := map[string][]map[string]any{}
	h.bc.mu.Lock()
	for key, refs := range h.bc.presence[tk] {
		for _, e := range refs {
			state[key] = append(state[key], e.meta)
		}
	}
	h.bc.mu.Unlock()
	ch.c.send(rtMsg{JoinRef: ch.joinRef, Topic: ch.topic, Event: evPresenceState, Payload: rawJSON(metas(state))})
}

// relayOut tells the project's other edge processes, when there are any.
func (h *rtHub) relayOut(ctx context.Context, m relayMsg) {
	if !h.bc.listening.Load() {
		return
	}
	h.bc.mu.Lock()
	peers := len(h.bc.peers)
	h.bc.mu.Unlock()
	if peers == 0 && m.Kind != "hi" && m.Kind != "bye" {
		return
	}
	p := h.e.lookup(h.ref)
	if p == nil {
		return
	}
	m.Node = h.e.node()
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	pool, err := h.e.dbPool(ctx, p)
	if err != nil {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if parts := splitRelay(m.Node, h.bc.partID.Add(1), b); len(parts) > 1 && len(parts) <= rtMaxParts {
		// The NOTIFYs of one transaction: nothing is written to a table.
		if _, err := pool.Exec(nctx, `SELECT pg_notify($1, p) FROM unnest($2::text[]) AS p`, chanBroadcast, parts); err != nil {
			h.e.cfg.Log.Warn("realtime notify", "ref", h.ref, "err", err)
		}
		return
	}
	if len(b) > rtNotifyMax {
		var id int64
		if err := pool.QueryRow(nctx, `INSERT INTO xb_realtime.relay (body) VALUES ($1) RETURNING id`, string(b)).Scan(&id); err != nil {
			h.e.cfg.Log.Warn("realtime relay", "ref", h.ref, "err", err)
			return
		}
		b, _ = json.Marshal(relayMsg{Node: m.Node, Kind: "ref", Relay: id})
	}
	if _, err := pool.Exec(nctx, `SELECT pg_notify($1, $2)`, chanBroadcast, string(b)); err != nil {
		h.e.cfg.Log.Warn("realtime notify", "ref", h.ref, "err", err)
	}
}

// splitRelay is b as NOTIFY payloads: b itself when it fits, else the
// parts of message id.
func splitRelay(node string, id int64, b []byte) []string {
	if len(b) <= rtNotifyMax {
		return []string{string(b)}
	}
	// size bytes in base64, with room for the rest of the part.
	size := max((rtNotifyMax-160-len(node))/4*3, 1024)
	n := (len(b) + size - 1) / size
	out := make([]string, 0, n)
	for i := range n {
		chunk := b[i*size : min((i+1)*size, len(b))]
		part, _ := json.Marshal(relayMsg{Node: node, Kind: "part", Relay: id, Part: i, Parts: n, Data: base64.StdEncoding.EncodeToString(chunk)})
		out = append(out, string(part))
	}
	return out
}

// joinPart keeps a part of another process's message, and returns the
// whole message once every part is in (listener goroutine only).
func (h *rtHub) joinPart(m relayMsg) ([]byte, bool) {
	s := &h.bc
	now := time.Now()
	for k, ps := range s.parts {
		if now.Sub(ps.at) > rtPartsWait {
			s.partBytes -= ps.size
			delete(s.parts, k)
		}
	}
	if m.Parts < 2 || m.Parts > rtMaxParts || m.Part < 0 || m.Part >= m.Parts {
		return nil, false
	}
	chunk, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil || s.partBytes+len(chunk) > rtPartsPending {
		return nil, false
	}
	k := m.Node + ":" + strconv.FormatInt(m.Relay, 10)
	ps := s.parts[k]
	if ps == nil {
		ps = &relayParts{at: now, data: make([][]byte, m.Parts)}
		s.parts[k] = ps
	}
	if len(ps.data) != m.Parts || ps.data[m.Part] != nil {
		return nil, false
	}
	ps.data[m.Part] = chunk
	ps.got++
	ps.size += len(chunk)
	s.partBytes += len(chunk)
	if ps.got < m.Parts {
		return nil, false
	}
	delete(s.parts, k)
	s.partBytes -= ps.size
	whole := make([]byte, 0, ps.size)
	for _, c := range ps.data {
		whole = append(whole, c...)
	}
	return whole, true
}

// relayIn handles another process's message.
func (h *rtHub) relayIn(ctx context.Context, q querier, payload string) {
	var m relayMsg
	if json.Unmarshal([]byte(payload), &m) != nil || m.Node == h.e.node() {
		return
	}
	if m.Kind == "part" {
		whole, ok := h.joinPart(m)
		if !ok {
			return
		}
		m = relayMsg{}
		if json.Unmarshal(whole, &m) != nil || m.Node == h.e.node() {
			return
		}
	}
	if m.Kind == "ref" {
		var body string
		if err := q.QueryRow(ctx, `SELECT body FROM xb_realtime.relay WHERE id = $1`, m.Relay).Scan(&body); err != nil {
			return
		}
		if json.Unmarshal([]byte(body), &m) != nil || m.Node == h.e.node() {
			return
		}
	}
	h.bc.mu.Lock()
	_, known := h.bc.peers[m.Node]
	if m.Kind == "bye" {
		delete(h.bc.peers, m.Node)
	} else {
		h.bc.peers[m.Node] = time.Now()
	}
	h.bc.mu.Unlock()
	switch m.Kind {
	case "hi":
		if m.First || !known {
			// A process that just started (or we hadn't heard): say hello
			// back so it knows to relay to us, and tell it who is here.
			h.relayOut(ctx, relayMsg{Kind: "hi"})
			h.sendState(ctx)
		}
	case "bye":
		h.dropNode(m.Node)
	case "bc":
		h.fanOut(m.Topic, m.Private, m.Payload, nil)
	case "pd":
		h.applyRemote(m.Node, m.Topic, m.Private, m.Joins, m.Leaves)
	case "pst":
		for tk, keys := range m.State {
			private := strings.HasPrefix(tk, "p:")
			topic := tk[2:]
			h.replaceNode(m.Node, topic, private, keys)
		}
	}
}

// hello announces this process; first asks the others for their presence.
func (h *rtHub) hello(ctx context.Context, _ querier, first bool) {
	h.relayOut(ctx, relayMsg{Kind: "hi", First: first})
}

func (h *rtHub) goodbye(ctx context.Context, _ querier) {
	h.relayOut(ctx, relayMsg{Kind: "bye"})
}

// sendState sends this process's presence on every topic.
func (h *rtHub) sendState(ctx context.Context) {
	state := map[string]map[string][]map[string]any{}
	h.bc.mu.Lock()
	for tk, keys := range h.bc.presence {
		for key, refs := range keys {
			for _, e := range refs {
				if e.ch == nil {
					continue
				}
				if state[tk] == nil {
					state[tk] = map[string][]map[string]any{}
				}
				state[tk][key] = append(state[tk][key], e.meta)
			}
		}
	}
	h.bc.mu.Unlock()
	// A topic at a time, so each message stays within what parts carry.
	for tk, keys := range state {
		h.relayOut(ctx, relayMsg{Kind: "pst", State: map[string]map[string][]map[string]any{tk: keys}})
	}
}

// applyRemote applies another process's presence diff.
func (h *rtHub) applyRemote(node, topic string, private bool, joins, leaves map[string][]map[string]any) {
	tk := topicKey(topic, private)
	h.bc.mu.Lock()
	if h.bc.presence[tk] == nil {
		h.bc.presence[tk] = map[string]map[string]*presenceEntry{}
	}
	keys := h.bc.presence[tk]
	for key, ms := range leaves {
		for _, meta := range ms {
			ref, _ := meta["phx_ref"].(string)
			if refs := keys[key]; refs != nil {
				delete(refs, ref)
				if len(refs) == 0 {
					delete(keys, key)
				}
			}
		}
	}
	for key, ms := range joins {
		for _, meta := range ms {
			ref, _ := meta["phx_ref"].(string)
			if ref == "" {
				continue
			}
			if keys[key] == nil {
				keys[key] = map[string]*presenceEntry{}
			}
			keys[key][ref] = &presenceEntry{node: node, key: key, meta: meta}
		}
	}
	if len(keys) == 0 {
		delete(h.bc.presence, tk)
	}
	h.bc.mu.Unlock()
	h.presenceDiff(topic, private, joins, leaves)
}

// replaceNode sets another process's whole presence on a topic.
func (h *rtHub) replaceNode(node, topic string, private bool, keys map[string][]map[string]any) {
	tk := topicKey(topic, private)
	have := map[string]bool{}
	h.bc.mu.Lock()
	for _, refs := range h.bc.presence[tk] {
		for ref, e := range refs {
			if e.node == node {
				have[ref] = true
			}
		}
	}
	h.bc.mu.Unlock()
	joins := map[string][]map[string]any{}
	for key, ms := range keys {
		for _, meta := range ms {
			if ref, _ := meta["phx_ref"].(string); ref != "" && !have[ref] {
				joins[key] = append(joins[key], meta)
			}
		}
	}
	if len(joins) > 0 {
		h.applyRemote(node, topic, private, joins, nil)
	}
}

// dropNode removes everything a gone process tracked.
func (h *rtHub) dropNode(node string) {
	type diff struct {
		topic   string
		private bool
		leaves  map[string][]map[string]any
	}
	var diffs []diff
	h.bc.mu.Lock()
	for tk, keys := range h.bc.presence {
		var leaves map[string][]map[string]any
		for key, refs := range keys {
			for ref, e := range refs {
				if e.node != node || e.ch != nil {
					continue
				}
				if leaves == nil {
					leaves = map[string][]map[string]any{}
				}
				leaves[key] = append(leaves[key], e.meta)
				delete(refs, ref)
			}
			if len(refs) == 0 {
				delete(keys, key)
			}
		}
		if len(keys) == 0 {
			delete(h.bc.presence, tk)
		}
		if leaves != nil {
			diffs = append(diffs, diff{topic: tk[2:], private: strings.HasPrefix(tk, "p:"), leaves: leaves})
		}
	}
	h.bc.mu.Unlock()
	for _, d := range diffs {
		h.presenceDiff(d.topic, d.private, nil, d.leaves)
	}
}

// expirePeers drops processes not heard for rtPeerExpiry (one that crashed
// takes its clients' presence with it, §6.5).
func (h *rtHub) expirePeers() {
	var gone []string
	h.bc.mu.Lock()
	for n, at := range h.bc.peers {
		if time.Since(at) > rtPeerExpiry {
			delete(h.bc.peers, n)
			gone = append(gone, n)
		}
	}
	h.bc.mu.Unlock()
	for _, n := range gone {
		h.dropNode(n)
	}
}

// restBroadcast sends broadcasts from a server: POST
// /realtime/v1/api/broadcast {"messages": [{topic, event, payload, private}]},
// with the secret key.
func (e *Edge) restBroadcast(c *call, req Request) {
	if c.r.Method != http.MethodPost {
		c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "send broadcasts with POST")
		return
	}
	if req.Role != "service" {
		c.fail(http.StatusForbidden, "secret_key_required", "sending broadcasts over HTTP needs the secret key")
		return
	}
	var in struct {
		Messages []struct {
			Topic   string          `json:"topic"`
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
			Private bool            `json:"private"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(c.w, c.r.Body, rtMaxFrame)).Decode(&in); err != nil || len(in.Messages) == 0 || len(in.Messages) > 100 {
		c.fail(http.StatusBadRequest, "bad_request", "send {\"messages\": [{topic, event, payload}]}, 1 to 100 of them")
		return
	}
	h := e.hub(c.p.cfg.Ref)
	payloads := make([]json.RawMessage, len(in.Messages))
	for i := range in.Messages {
		m := &in.Messages[i]
		if m.Topic == "" || m.Event == "" {
			c.fail(http.StatusBadRequest, "bad_request", "each message needs a topic and an event")
			return
		}
		if !strings.HasPrefix(m.Topic, rtTopicPrefix) {
			m.Topic = rtTopicPrefix + m.Topic
		}
		if len(m.Payload) == 0 {
			m.Payload = json.RawMessage(`{}`)
		}
		payloads[i] = rawJSON(map[string]any{"type": "broadcast", "event": m.Event, "payload": m.Payload})
		if len(payloads[i]) > rtMaxMessage {
			c.fail(http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a broadcast is at most %d KB", rtMaxMessage>>10))
			return
		}
	}
	for i, m := range in.Messages {
		if reason := h.budget(c.r.Context(), m.Topic, len(payloads[i])); reason != "" {
			// Those before it were sent.
			c.w.Header().Set("Retry-After", "1")
			c.json(http.StatusTooManyRequests, map[string]any{"sent": i,
				"error": Error{Code: "rate_limited", Message: strings.TrimPrefix(reason, "rate_limited: "), RequestID: c.id}})
			return
		}
		topic, payload := m.Topic, payloads[i]
		h.fanOut(topic, m.Private, payload, nil)
		h.counted(1)
		h.relayOut(c.r.Context(), relayMsg{Kind: "bc", Topic: topic, Private: m.Private, Payload: payload})
		h.persist(c.r.Context(), topic, m.Private, m.Event, m.Payload, nil)
	}
	c.json(http.StatusAccepted, map[string]any{"sent": len(in.Messages)})
}

// history reads a topic's persisted broadcasts: GET
// /realtime/v1/history/<topic>?after=<id>&limit=. That is the public
// channel's; private=true reads the private channel's, which needs the
// caller's read policy (or the secret key).
func (e *Edge) history(c *call, req Request, name string) {
	name = strings.TrimPrefix(name, rtTopicPrefix)
	q := c.r.URL.Query()
	private := q.Get("private") == "true"
	if private {
		a, err := e.channelAccess(c.r.Context(), c.p, req, rtTopicPrefix+name)
		if err != nil {
			e.dbError(c, err)
			return
		}
		if !a.readBroadcast {
			c.fail(http.StatusForbidden, "not_allowed", "no policy on xb_realtime.channel_access lets this caller read the topic")
			return
		}
	}
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	pool, err := e.dbPool(c.r.Context(), c.p)
	if err != nil {
		e.dbError(c, err)
		return
	}
	rows, err := pool.Query(c.r.Context(), `SELECT id, event, payload, sender, at FROM xb_realtime.broadcast_history
		WHERE topic = $1 AND private = $2 AND id > $3 ORDER BY id LIMIT $4`, name, private, after, limit)
	if err != nil {
		e.dbError(c, err)
		return
	}
	type item struct {
		ID      int64           `json:"id"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
		Sender  *uuid.UUID      `json:"sender,omitempty"`
		At      time.Time       `json:"at"`
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var it item
		var payload []byte
		err := r.Scan(&it.ID, &it.Event, &payload, &it.Sender, &it.At)
		it.Payload = payload
		return it, err
	})
	if err != nil {
		e.dbError(c, err)
		return
	}
	if items == nil {
		items = []item{}
	}
	c.json(http.StatusOK, map[string]any{"topic": name, "private": private, "items": items})
}
