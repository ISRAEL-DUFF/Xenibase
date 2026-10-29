package edge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// Realtime tests against a project database (XENIBASE_TEST_DATABASE_URL).
// The edge logs in as the test's login and switches to the request roles,
// as with a feed from before the request roles had logins, and the database
// has the parts of xb_realtime the edge uses, as schema versions 5 and 9 in
// internal/services/schema.go make them.

type rtProject struct {
	e        *Edge
	pc       edgeapi.Project
	db       *pgxpool.Pool // the platform
	pub, sec string
	der      []byte
}

const rtDomain = "edge.test"

// rtFixture is the xb_realtime the edge reads and writes.
const rtFixture = `
CREATE SCHEMA xb_realtime;
GRANT USAGE ON SCHEMA xb_realtime TO {{anon}}, {{user}}, {{service}};
CREATE TABLE xb_realtime.tables (schema_name text, table_name text, pk_columns text[], PRIMARY KEY (schema_name, table_name));
CREATE TABLE xb_realtime.outbox (id bigserial PRIMARY KEY, xid xid8 NOT NULL DEFAULT pg_current_xact_id(), schema_name text NOT NULL,
  table_name text NOT NULL, op text NOT NULL, record jsonb, old_record jsonb, at timestamptz NOT NULL DEFAULT now());
CREATE TABLE xb_realtime.channel_access (id bigserial PRIMARY KEY, topic text NOT NULL,
  extension text NOT NULL CHECK (extension IN ('broadcast', 'presence')), created_at timestamptz NOT NULL DEFAULT now());
CREATE FUNCTION xb_realtime.probe(topic text, extension text) RETURNS bigint LANGUAGE sql VOLATILE SECURITY DEFINER
  SET search_path = pg_catalog, xb_realtime AS
  $$ INSERT INTO xb_realtime.channel_access (topic, extension) VALUES (topic, extension) RETURNING id $$;
GRANT EXECUTE ON FUNCTION xb_realtime.probe(text, text) TO {{anon}}, {{user}}, {{service}};
GRANT SELECT, INSERT ON xb_realtime.channel_access TO {{anon}}, {{user}}, {{service}};
GRANT USAGE ON SEQUENCE xb_realtime.channel_access_id_seq TO {{anon}}, {{user}}, {{service}};
ALTER TABLE xb_realtime.channel_access ENABLE ROW LEVEL SECURITY;
CREATE POLICY xenibase_probe ON xb_realtime.channel_access FOR ALL TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE TABLE xb_realtime.persisted_topics (topic text PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE xb_realtime.broadcast_history (id bigserial PRIMARY KEY, topic text NOT NULL, event text NOT NULL,
  payload jsonb NOT NULL, sender uuid, at timestamptz NOT NULL DEFAULT now());
CREATE TABLE xb_realtime.relay (id bigserial PRIMARY KEY, body text NOT NULL, at timestamptz NOT NULL DEFAULT now());
ALTER TABLE xb_realtime.broadcast_history ADD COLUMN IF NOT EXISTS private boolean NOT NULL DEFAULT true;
CREATE INDEX IF NOT EXISTS broadcast_history_topic_private ON xb_realtime.broadcast_history (topic, private, id);
GRANT USAGE ON SCHEMA public TO {{anon}}, {{user}}, {{service}};
`

func newRTProject(t *testing.T) *rtProject {
	t.Helper()
	admin := os.Getenv(storetest.EnvVar)
	if admin == "" {
		t.Skipf("%s not set; skipping test that needs Postgres", storetest.EnvVar)
	}
	ctx := context.Background()
	ac, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	// The roles are the cluster's: dropped once the database is.
	ref := "rt" + randRef()[:8]
	roles := map[string]string{"{{anon}}": ref + "_anon", "{{user}}": ref + "_user", "{{service}}": ref + "_service"}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		for _, r := range roles {
			_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+r)
		}
	})
	db := storetest.NewEmpty(t)
	fixture := rtFixture
	for k, r := range roles {
		if _, err := db.Exec(ctx, "CREATE ROLE "+r+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		fixture = strings.ReplaceAll(fixture, k, r)
	}
	if _, err := db.Exec(ctx, fixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	der, jwk, err := jwtes.Generate("k1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(jwk)
	rp := &rtProject{db: db, pub: "xb_pub_" + ref, sec: "xb_sec_" + ref, der: der}
	rp.pc = edgeapi.Project{Ref: ref, ProjectID: uuid.New(), OrgID: uuid.New(), State: edgeapi.StateActive, Version: 1,
		Database: db.Config().ConnConfig.Database, EdgeUser: ac.User, Password: ac.Password,
		PoolerHost: ac.Host, PoolerPort: int(ac.Port), SessionHost: ac.Host, SessionPort: int(ac.Port),
		AnonRole: roles["{{anon}}"], UserRole: roles["{{user}}"], ServiceRole: roles["{{service}}"],
		Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey(rp.pub)},
			{ID: uuid.New(), Kind: edgeapi.KindSecret, Hash: edgeapi.HashKey(rp.sec)}},
		JWKs:     []json.RawMessage{raw},
		Realtime: edgeapi.RealtimeConfig{MaxChangesPerSecond: 200, MaxGroups: 100}}
	rp.e = New(Config{Domain: rtDomain, PoolerSSLMode: "disable"})
	rp.e.apply([]edgeapi.Project{rp.pc})
	rp.e.ready = true
	t.Cleanup(rp.e.closePools)
	return rp
}

// token is a user's access token for the project, expiring in d.
func (rp *rtProject) token(t *testing.T, sub string, d time.Duration) string {
	t.Helper()
	s, err := jwtes.Sign(rp.der, "k1", map[string]any{"sub": sub, "role": "user", "aud": rp.pc.Ref,
		"exp": float64(time.Now().Add(d).Unix())})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (rp *rtProject) project(t *testing.T) *project {
	t.Helper()
	p := rp.e.lookup(rp.pc.Ref)
	if p == nil {
		t.Fatal("the project isn't in the edge's feed")
	}
	return p
}

// get is a GET through the gateway with a key and, if given, a token.
func (rp *rtProject) get(path, key, token string) (int, string) {
	r := httptest.NewRequest(http.MethodGet, "http://edge"+path, nil)
	r.Host = rp.pc.Ref + "." + rtDomain
	r.Header.Set("apikey", key)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	rp.e.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// TestRealtimeHistoryKeepsPrivateApart: a persisted topic's private
// broadcasts are read only through its read policy, never as the public
// channel's history; rows that don't say which they were count as private.
func TestRealtimeHistoryKeepsPrivateApart(t *testing.T) {
	rp := newRTProject(t)
	ctx := context.Background()
	member := uuid.New()
	for _, st := range []string{
		`INSERT INTO xb_realtime.persisted_topics VALUES ('vip')`,
		// From before the flag: the column's default.
		`INSERT INTO xb_realtime.broadcast_history (topic, event, payload) VALUES ('vip', 'legacy', '{}')`,
		`CREATE POLICY members ON xb_realtime.channel_access FOR SELECT TO ` + rp.pc.UserRole +
			` USING (topic = 'vip' AND current_setting('xb.claims', true)::jsonb ->> 'sub' = '` + member.String() + `')`,
	} {
		if _, err := rp.db.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	h := rp.e.hub(rp.pc.Ref)
	rc := testConn(h, Request{Role: "user"})
	priv := &rtChannel{c: rc, topic: "realtime:vip", private: true, userID: &member}
	h.persist(ctx, "realtime:vip", true, "secret", json.RawMessage(`{"msg":"for members"}`), priv)
	h.persist(ctx, "realtime:vip", false, "hello", json.RawMessage(`{"msg":"for anyone"}`), nil)

	events := func(body string) []string {
		var out struct {
			Items []struct {
				Event string `json:"event"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		var ev []string
		for _, it := range out.Items {
			ev = append(ev, it.Event)
		}
		return ev
	}
	for _, c := range []struct {
		name, path, key, token string
		code                   int
		want                   string
	}{
		{"the publishable key", "/realtime/v1/history/vip", rp.pub, "", 200, "hello"},
		{"a member without private=true", "/realtime/v1/history/vip", rp.pub, rp.token(t, member.String(), time.Hour), 200, "hello"},
		{"the publishable key, private", "/realtime/v1/history/vip?private=true", rp.pub, "", 403, ""},
		{"a non-member, private", "/realtime/v1/history/vip?private=true", rp.pub, rp.token(t, uuid.NewString(), time.Hour), 403, ""},
		{"a member, private", "/realtime/v1/history/vip?private=true", rp.pub, rp.token(t, member.String(), time.Hour), 200, "legacy,secret"},
		{"the secret key, private", "/realtime/v1/history/vip?private=true", rp.sec, "", 200, "legacy,secret"},
		{"the secret key", "/realtime/v1/history/vip", rp.sec, "", 200, "hello"},
	} {
		code, body := rp.get(c.path, c.key, c.token)
		if code != c.code {
			t.Fatalf("%s: %d %s", c.name, code, body)
		}
		if got := strings.Join(events(body), ","); code == 200 && got != c.want {
			t.Fatalf("%s: history %q, want %q (%s)", c.name, got, c.want, body)
		}
	}
	var private bool
	if err := rp.db.QueryRow(ctx, `SELECT private FROM xb_realtime.broadcast_history WHERE event = 'hello'`).Scan(&private); err != nil || private {
		t.Fatalf("the public broadcast's row: private=%v %v", private, err)
	}
}

// changesOn adds a subscription to every change of public.<table> on a new
// channel of rc.
func changesOn(rc *rtConn, topic, table string) (*rtChannel, *rtSub) {
	ch := &rtChannel{c: rc, topic: topic, req: rc.base, readBroadcast: true, writeBroadcast: true, readPresence: true, writePresence: true}
	s := &rtSub{id: rtSubIDs.Add(1), ch: ch, event: "*", schema: "public", table: table, seen: newPKSet(10000)}
	ch.subs = []*rtSub{s}
	rc.mu.Lock()
	rc.channels[topic] = ch
	rc.mu.Unlock()
	h := rc.hub
	h.mu.Lock()
	h.channels[ch] = struct{}{}
	k := "public." + table
	if h.subs[k] == nil {
		h.subs[k] = map[*rtSub]struct{}{}
	}
	h.subs[k][s] = struct{}{}
	h.mu.Unlock()
	return ch, s
}

// inserted is an outbox row for an insert of row into public.<table>.
func inserted(table string, row map[string]any) change {
	b, _ := json.Marshal(row)
	rec := decodeRow(b)
	return change{schema: "public", table: table, op: "INSERT", record: rec, at: time.Now(), pk: pkKey([]string{"id"}, rec)}
}

// TestRealtimeChangesStopWithoutRLS: a subscription made while its table
// had row-level security is closed, not sent every row, once the owner
// turns it off.
func TestRealtimeChangesStopWithoutRLS(t *testing.T) {
	rp := newRTProject(t)
	ctx := context.Background()
	for _, st := range []string{
		`CREATE TABLE public.items (id int PRIMARY KEY, owner text NOT NULL)`,
		`GRANT SELECT ON public.items TO ` + rp.pc.AnonRole,
		`ALTER TABLE public.items ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY mine ON public.items FOR SELECT TO ` + rp.pc.AnonRole + ` USING (owner = 'anon')`,
		`INSERT INTO public.items VALUES (1, 'anon'), (2, 'someone')`,
	} {
		if _, err := rp.db.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	p := rp.project(t)
	h := rp.e.hub(rp.pc.Ref)
	rc := testConn(h, Request{Role: "anon"})
	ch, _ := changesOn(rc, "realtime:items", "items")
	keys := map[string][]string{"public.items": {"id"}}
	changes := func(got []rtMsg) (n int, closed bool) {
		for _, m := range got {
			switch {
			case m.Event == evPostgresChanges:
				n++
			case m.Event == evSystem && strings.Contains(string(m.Payload), "no longer has row-level security"):
				closed = true
			}
		}
		return n, closed
	}

	h.deliver(ctx, p, keys, []change{inserted("items", map[string]any{"id": 1, "owner": "anon"}), inserted("items", map[string]any{"id": 2, "owner": "someone"})})
	if n, closed := changes(received(t, rc)); n != 1 || closed {
		t.Fatalf("with row-level security: %d changes (want only the subscriber's own row), closed=%v", n, closed)
	}
	if _, err := rp.db.Exec(ctx, `ALTER TABLE public.items DISABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	h.deliver(ctx, p, keys, []change{inserted("items", map[string]any{"id": 2, "owner": "someone"})})
	if n, closed := changes(received(t, rc)); n != 0 || !closed {
		t.Fatalf("without row-level security: %d changes, closed=%v", n, closed)
	}
	h.mu.Lock()
	left, own := len(h.subs["public.items"]), len(ch.subs)
	h.mu.Unlock()
	if left != 0 || own != 0 {
		t.Fatalf("the subscription is still there: hub %d, channel %d", left, own)
	}
	// The channel itself stays, for its broadcasts.
	rc.mu.Lock()
	still := rc.channels["realtime:items"] == ch
	rc.mu.Unlock()
	if !still {
		t.Fatal("the channel was closed")
	}
}

// TestRealtimeBroadcastsSpareTheDatabase: a large broadcast reaches the
// project's other edge processes in NOTIFY parts, with no xb_realtime.relay
// row, and a persisted topic keeps at most 256 KB of history a minute.
func TestRealtimeBroadcastsSpareTheDatabase(t *testing.T) {
	rp := newRTProject(t)
	ctx := context.Background()
	if _, err := rp.db.Exec(ctx, `INSERT INTO xb_realtime.persisted_topics VALUES ('log')`); err != nil {
		t.Fatal(err)
	}
	h := rp.e.hub(rp.pc.Ref)

	// Another process's listening connection, and this one knowing it.
	listener, err := pgx.ConnectConfig(ctx, rp.db.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(ctx)
	if _, err := listener.Exec(ctx, `LISTEN `+chanBroadcast); err != nil {
		t.Fatal(err)
	}
	h.bc.listening.Store(true)
	h.bc.mu.Lock()
	h.bc.peers["edge-other-1"] = time.Now()
	h.bc.mu.Unlock()
	other := New(Config{Domain: rtDomain, PoolerSSLMode: "disable", Name: "edge-other"})
	other.apply([]edgeapi.Project{rp.pc})
	oh := other.hub(rp.pc.Ref)
	to := testConn(oh, Request{Role: "anon"})
	testChannel(to, "realtime:log")

	from := testConn(h, Request{Role: "anon"})
	testChannel(from, "realtime:log")
	sent := 0
	for i := 0; i < 12; i++ {
		from.handle(ctx, msg("realtime:log", evBroadcast, broadcastOf(30<<10)))
		if r, _ := replies(t, from); len(r) == 0 {
			sent++
		} else if !strings.Contains(r[0], "broadcast history") {
			t.Fatalf("broadcast %d: %v", i, r)
		}
	}
	if sent == 0 || sent > rtHistoryBytesPerMinute/(30<<10) {
		t.Fatalf("%d of 12 30 KB broadcasts on a persisted topic sent in a minute", sent)
	}
	var kept, relayed int
	if err := rp.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM xb_realtime.broadcast_history), (SELECT count(*) FROM xb_realtime.relay)`).Scan(&kept, &relayed); err != nil {
		t.Fatal(err)
	}
	if kept != sent || relayed != 0 {
		t.Fatalf("history rows %d (want %d), relay rows %d", kept, sent, relayed)
	}
	// What the other process hears, joined.
	delivered := 0
	for delivered < sent {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := listener.WaitForNotification(wctx)
		cancel()
		if err != nil {
			t.Fatalf("%d of %d broadcasts heard: %v", delivered, sent, err)
		}
		if len(n.Payload) > rtNotifyMax {
			t.Fatalf("a NOTIFY of %d bytes", len(n.Payload))
		}
		oh.relayIn(ctx, nil, n.Payload)
		_, b := replies(t, to)
		delivered += b
	}
}
