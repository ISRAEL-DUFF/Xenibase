package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// realtimeProject is a project with backend services, two users, and a
// todos table each user sees only their own rows of, with realtime on.
type realtimeProject struct {
	e          *testenv.Env
	ed         *testenv.Edge
	ref        string
	pid        uuid.UUID
	db         string
	pub, sec   string
	owner      string // the project owner's pooled URL
	alice, bob string
	aliceID    uuid.UUID
	bobID      uuid.UUID
}

func newRealtimeProject(t *testing.T, name string) *realtimeProject {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	creds := e.CreateProject(name)
	pid := creds.Project.Id
	p, err := store.New(e.DB).GetProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	var en gen.BackendServicesEnabled
	if code := e.Do("POST", "/api/v1/projects/"+pid.String()+"/services", nil, &en); code != http.StatusAccepted {
		t.Fatalf("enable: %d", code)
	}
	if op := e.WaitOperation(en.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	rp := &realtimeProject{e: e, ref: *en.Services.Ref, pid: pid, db: p.DbName, owner: creds.Connection.PooledUrl}
	for _, k := range en.Keys {
		if k.Key.Kind == gen.ApiKeyKindPublishable {
			rp.pub = k.Value
		} else {
			rp.sec = k.Value
		}
	}
	rp.ed = e.StartEdge()
	waitFor(t, 30*time.Second, "the edge reaches the project's database", func() bool {
		code, _, _ := rp.ed.Do(rp.ref, "GET", "/data/v1/health", nil, "apikey", rp.pub)
		return code == 200
	})
	api := authAPI{t: t, ed: rp.ed, ref: rp.ref, key: rp.sec}
	user := func(email string) (string, uuid.UUID) {
		if r := api.post("/auth/v1/admin/users", fmt.Sprintf(`{"email":%q,"password":"password-123456","email_confirm":true}`, email)); r.Code != 201 {
			t.Fatalf("create %s: %d %s", email, r.Code, r.Body)
		}
		r := authAPI{t: t, ed: rp.ed, ref: rp.ref, key: rp.pub}.post("/auth/v1/signin/password", fmt.Sprintf(`{"email":%q,"password":"password-123456"}`, email))
		if r.Code != 200 {
			t.Fatalf("sign in %s: %d %s", email, r.Code, r.Body)
		}
		return r.Session.AccessToken, r.Session.User.ID
	}
	rp.alice, rp.aliceID = user("alice@example.com")
	rp.bob, rp.bobID = user("bob@example.com")
	app := rp.ownerConn(t)
	defer app.Close(ctx)
	for _, st := range []string{
		`CREATE TABLE todos (id serial PRIMARY KEY, owner uuid NOT NULL, body text NOT NULL, done boolean NOT NULL DEFAULT false)`,
		`ALTER TABLE todos ENABLE ROW LEVEL SECURITY`,
		fmt.Sprintf(`CREATE POLICY own_todos ON todos FOR ALL TO %q USING (owner = xb_auth.uid()) WITH CHECK (owner = xb_auth.uid())`,
			store.UserRole(p.DbName)),
		`SELECT xb_realtime.enable('todos')`,
	} {
		if _, err := app.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	return rp
}

func (rp *realtimeProject) ownerConn(t *testing.T) *pgx.Conn {
	t.Helper()
	return rp.e.MustConnect(rp.owner)
}

// TestRealtimeCapture: enabling a table captures its committed changes in
// the outbox, and a rolled-back change leaves nothing.
func TestRealtimeCapture(t *testing.T) {
	rp := newRealtimeProject(t, "rt-capture")
	ctx := context.Background()
	app := rp.ownerConn(t)
	defer app.Close(ctx)

	if _, err := app.Exec(ctx, `SELECT xb_realtime.enable('xb_realtime.outbox')`); err == nil {
		t.Fatal("enabled realtime on a platform table")
	}
	if _, err := app.Exec(ctx, `CREATE TABLE nokey (x int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `SELECT xb_realtime.enable('nokey')`); err == nil {
		t.Fatal("enabled realtime on a table without a primary key")
	}
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'rolled back')`, rp.aliceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'kept')`, rp.aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `UPDATE todos SET done = true WHERE body = 'kept'`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `DELETE FROM todos WHERE body = 'kept'`); err != nil {
		t.Fatal(err)
	}
	p, err := store.New(rp.e.DB).GetProject(ctx, rp.pid)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := rp.e.Service.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		t.Fatal(err)
	}
	defer adm.Close(ctx)
	rows, err := adm.Query(ctx, `SELECT op, coalesce(record ->> 'body', ''), coalesce(old_record::text, '-') FROM xb_realtime.outbox ORDER BY xid, id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var op, body, old string
		if err := rows.Scan(&op, &body, &old); err != nil {
			t.Fatal(err)
		}
		got = append(got, op+" "+body+" "+old)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := []string{`INSERT kept -`, `UPDATE kept {"id": 2}`, `DELETE  {"id": 2}`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("outbox: %q, want %q", got, want)
	}
	if _, err := app.Exec(ctx, `SELECT xb_realtime.disable('todos')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'after')`, rp.aliceID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := adm.QueryRow(ctx, `SELECT count(*) FROM xb_realtime.outbox WHERE record ->> 'body' = 'after'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("captured after disable: %d %v", n, err)
	}

	// ---- The dashboard's API ---------------------------------------------------
	base := "/api/v1/projects/" + rp.pid.String() + "/realtime"
	if code := rp.e.Do("PUT", base+"/tables/public/todos", map[string]any{"enabled": true}, nil); code != http.StatusNoContent {
		t.Fatalf("turn on todos: %d", code)
	}
	if code := rp.e.Do("PUT", base+"/tables/public/nokey", map[string]any{"enabled": true}, nil); code != http.StatusBadRequest {
		t.Fatalf("a table without a primary key: %d", code)
	}
	if code := rp.e.Do("PUT", base+"/tables/public/nope", map[string]any{"enabled": true}, nil); code != http.StatusNotFound {
		t.Fatalf("no such table: %d", code)
	}
	if code := rp.e.Do("PUT", base+"/persisted-topics", map[string]any{"topics": []string{"room1", "room2"}}, nil); code != http.StatusNoContent {
		t.Fatalf("history topics: %d", code)
	}
	var o gen.RealtimeOverview
	if code := rp.e.Do("GET", base, nil, &o); code != http.StatusOK {
		t.Fatalf("overview: %d", code)
	}
	tables := map[string]gen.RealtimeTable{}
	for _, tb := range o.Tables {
		tables[tb.Schema+"."+tb.Table] = tb
	}
	if !tables["public.todos"].Enabled || !tables["public.todos"].Rls || tables["public.nokey"].Enabled || tables["public.nokey"].HasPrimaryKey ||
		fmt.Sprint(o.PersistedTopics) != "[room1 room2]" {
		t.Fatalf("overview: %+v", o)
	}

	// ---- The CLI -------------------------------------------------------------------
	bin := buildCLI(t)
	token := rp.e.CreateToken(map[string]any{"name": "rt", "org_id": rp.e.OrgID, "scopes": []string{"read", "write"}})
	env := []string{"XENIBASE_SERVER=" + rp.e.URL, "XENIBASE_TOKEN=" + token, "XENIBASE_CONFIG_DIR=" + t.TempDir()}
	pid := rp.pid.String()
	for _, args := range [][]string{{"realtime", "disable", pid, "todos"}, {"realtime", "enable", pid, "public.todos"}, {"realtime", "history", pid, "lobby"}} {
		if r := runCLI(t, bin, env, args...); r.code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", args, r.code, r.stdout, r.stderr)
		}
	}
	if r := runCLI(t, bin, env, "realtime", "status", pid); r.code != 0 || !strings.Contains(r.stdout, "public.todos") ||
		!strings.Contains(r.stdout, "no primary key") || !strings.Contains(r.stdout, "History kept for: lobby") {
		t.Fatalf("status: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := runCLI(t, bin, env, "realtime", "enable", pid, "nokey"); r.code == 0 || !strings.Contains(r.stderr, "primary key") {
		t.Fatalf("enable a table without a key: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// rtClient is a realtime WebSocket client speaking the Phoenix protocol
// (vsn 1.0.0), as supabase-js does.
type rtClient struct {
	t    *testing.T
	ws   *websocket.Conn
	in   chan rtMessage
	ref  int
	done chan struct{}
}

type rtMessage struct {
	Topic   string          `json:"topic"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Ref     *string         `json:"ref"`
	JoinRef *string         `json:"join_ref"`
}

// connectRT opens a WebSocket to ed for project ref with key.
func connectRT(t *testing.T, ed *testenv.Edge, ref, key string) *rtClient {
	t.Helper()
	return connectRTAt(t, ed.URL, ref, key)
}

// connectRTAt opens a WebSocket to the edge at base for project ref.
func connectRTAt(t *testing.T, base, ref, key string) *rtClient {
	t.Helper()
	u := strings.Replace(base, "http://", "ws://", 1) + "/realtime/v1/websocket?vsn=1.0.0&apikey=" + url.QueryEscape(key)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{Host: ref + "." + testenv.EdgeDomain})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		code := 0
		if res != nil {
			code = res.StatusCode
		}
		t.Fatalf("dial realtime: %d %v", code, err)
	}
	ws.SetReadLimit(1 << 20)
	c := &rtClient{t: t, ws: ws, in: make(chan rtMessage, 1000), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m rtMessage
			if json.Unmarshal(b, &m) == nil {
				c.in <- m
			}
		}
	}()
	t.Cleanup(func() { _ = ws.Close(websocket.StatusNormalClosure, "") })
	return c
}

func (c *rtClient) send(topic, event string, payload any) string {
	c.t.Helper()
	c.ref++
	ref := fmt.Sprint(c.ref)
	b, _ := json.Marshal(map[string]any{"topic": topic, "event": event, "payload": payload, "ref": ref, "join_ref": ref})
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatalf("send %s: %v", event, err)
	}
	return ref
}

// next waits for a message matching ok, failing after d.
func (c *rtClient) next(d time.Duration, what string, ok func(rtMessage) bool) rtMessage {
	c.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-c.in:
			if ok(m) {
				return m
			}
		case <-deadline:
			c.t.Fatalf("no %s within %s", what, d)
		}
	}
}

// quiet fails if a message matching ok arrives within d.
func (c *rtClient) quiet(d time.Duration, what string, ok func(rtMessage) bool) {
	c.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-c.in:
			if ok(m) {
				c.t.Fatalf("unexpected %s: %s %s", what, m.Event, m.Payload)
			}
		case <-deadline:
			return
		}
	}
}

// join joins topic with config and waits for the reply.
func (c *rtClient) join(topic string, config map[string]any, token string) (status string, response json.RawMessage) {
	c.t.Helper()
	payload := map[string]any{"config": config}
	if token != "" {
		payload["access_token"] = token
	}
	ref := c.send(topic, "phx_join", payload)
	m := c.next(10*time.Second, "join reply", func(m rtMessage) bool {
		return m.Event == "phx_reply" && m.Ref != nil && *m.Ref == ref
	})
	var r struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	_ = json.Unmarshal(m.Payload, &r)
	return r.Status, r.Response
}

// change is a postgres_changes message's data.
type rtChange struct {
	IDs  []int64 `json:"ids"`
	Data struct {
		Type      string         `json:"type"`
		Table     string         `json:"table"`
		Record    map[string]any `json:"record"`
		OldRecord map[string]any `json:"old_record"`
	} `json:"data"`
}

func isChange(op, body string) func(rtMessage) bool {
	return func(m rtMessage) bool {
		if m.Event != "postgres_changes" {
			return false
		}
		var c rtChange
		_ = json.Unmarshal(m.Payload, &c)
		if c.Data.Type != op {
			return false
		}
		return body == "" || c.Data.Record["body"] == body
	}
}

func anyChange(m rtMessage) bool { return m.Event == "postgres_changes" }

// TestRealtime covers V4-M34's done-when (V4 §6): two users subscribed to
// the same table each receive only the rows their policies allow, and a
// rolled-back insert produces nothing. Around it: updates and deletes
// (deletes reach only those who saw the row), filters, refused
// subscriptions, heartbeats, broadcast and presence across two edge
// processes, private channels, history, and the change rate cap.
func TestRealtime(t *testing.T) {
	rp := newRealtimeProject(t, "rt-app")
	ctx := context.Background()
	app := rp.ownerConn(t)
	defer app.Close(ctx)
	todos := map[string]any{"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "todos"}}}

	alice := connectRT(t, rp.ed, rp.ref, rp.pub)
	bob := connectRT(t, rp.ed, rp.ref, rp.pub)
	if st, resp := alice.join("realtime:todos", todos, rp.alice); st != "ok" || !strings.Contains(string(resp), `"table":"todos"`) {
		t.Fatalf("alice joins: %s %s", st, resp)
	}
	if st, resp := bob.join("realtime:todos", todos, rp.bob); st != "ok" {
		t.Fatalf("bob joins: %s %s", st, resp)
	}
	alice.next(5*time.Second, "subscribed", func(m rtMessage) bool {
		return m.Event == "system" && strings.Contains(string(m.Payload), "Subscribed")
	})

	// ---- Each sees only their own rows -------------------------------------------
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'alice 1'), ($2, 'bob 1')`, rp.aliceID, rp.bobID); err != nil {
		t.Fatal(err)
	}
	alice.next(10*time.Second, "alice's insert", isChange("INSERT", "alice 1"))
	bob.next(10*time.Second, "bob's insert", isChange("INSERT", "bob 1"))
	alice.quiet(time.Second, "bob's row at alice", isChange("INSERT", "bob 1"))
	bob.quiet(100*time.Millisecond, "alice's row at bob", isChange("INSERT", "alice 1"))

	// ---- A rolled-back insert produces nothing -----------------------------------
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'never')`, rp.aliceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	alice.quiet(2*time.Second, "a rolled-back insert", anyChange)

	// ---- Updates and deletes -------------------------------------------------------
	if _, err := app.Exec(ctx, `UPDATE todos SET done = true WHERE body = 'alice 1'`); err != nil {
		t.Fatal(err)
	}
	m := alice.next(10*time.Second, "alice's update", isChange("UPDATE", "alice 1"))
	var ch rtChange
	_ = json.Unmarshal(m.Payload, &ch)
	if ch.Data.Record["done"] != true || ch.Data.OldRecord["id"] == nil {
		t.Fatalf("update payload: %s", m.Payload)
	}
	// Carol (another publishable-key client, as bob) joins after alice's
	// row exists: its delete must not reach her.
	carol := connectRT(t, rp.ed, rp.ref, rp.pub)
	if st, _ := carol.join("realtime:todos", todos, rp.bob); st != "ok" {
		t.Fatal("carol joins")
	}
	if _, err := app.Exec(ctx, `DELETE FROM todos WHERE body = 'alice 1'`); err != nil {
		t.Fatal(err)
	}
	m = alice.next(10*time.Second, "alice's delete", isChange("DELETE", ""))
	_ = json.Unmarshal(m.Payload, &ch)
	if ch.Data.OldRecord["id"] == nil {
		t.Fatalf("delete payload: %s", m.Payload)
	}
	bob.quiet(time.Second, "alice's delete at bob", isChange("DELETE", ""))
	carol.quiet(100*time.Millisecond, "alice's delete at a later subscriber", isChange("DELETE", ""))

	// ---- Filters --------------------------------------------------------------------
	filtered := map[string]any{"postgres_changes": []map[string]any{{"event": "INSERT", "schema": "public", "table": "todos", "filter": "body=eq.wanted"}}}
	if st, _ := alice.join("realtime:wanted", filtered, rp.alice); st != "ok" {
		t.Fatal("filtered join")
	}
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'unwanted'), ($1, 'wanted')`, rp.aliceID); err != nil {
		t.Fatal(err)
	}
	alice.next(10*time.Second, "the filtered insert", func(m rtMessage) bool {
		return m.Topic == "realtime:wanted" && isChange("INSERT", "wanted")(m)
	})
	alice.quiet(time.Second, "an insert the filter excludes", func(m rtMessage) bool {
		return m.Topic == "realtime:wanted" && isChange("INSERT", "unwanted")(m)
	})

	// ---- Refused subscriptions ------------------------------------------------------
	if _, err := app.Exec(ctx, `CREATE TABLE notes (id serial PRIMARY KEY, body text)`); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]map[string]any{
		"realtime off": {"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "notes"}}},
		"no table":     {"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "nope"}}},
		"bad filter":   {"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "todos", "filter": "body~x"}}},
		"bad event":    {"postgres_changes": []map[string]any{{"event": "TRUNCATE", "schema": "public", "table": "todos"}}},
	} {
		if st, resp := alice.join("realtime:bad-"+strings.ReplaceAll(name, " ", "-"), cfg, rp.alice); st != "error" {
			t.Fatalf("%s: %s %s", name, st, resp)
		}
	}
	if _, err := app.Exec(ctx, `SELECT xb_realtime.enable('notes')`); err != nil {
		t.Fatal(err)
	}
	if st, resp := alice.join("realtime:notes", map[string]any{"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "notes"}}}, rp.alice); st != "error" ||
		!strings.Contains(string(resp), "row-level security") {
		t.Fatalf("a table without row-level security: %s %s", st, resp)
	}
	if st, resp := alice.join("realtime:expired", todos, "not.a.token"); st != "error" || !strings.Contains(string(resp), "invalid token") {
		t.Fatalf("a bad token: %s %s", st, resp)
	}

	// ---- Heartbeats ------------------------------------------------------------------
	ref := alice.send("phoenix", "heartbeat", map[string]any{})
	alice.next(5*time.Second, "heartbeat reply", func(m rtMessage) bool { return m.Event == "phx_reply" && m.Ref != nil && *m.Ref == ref })

	// ---- Row-level security turned off after subscribing ----------------------------
	if _, err := app.Exec(ctx, `ALTER TABLE todos DISABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) VALUES ($1, 'bob after rls')`, rp.bobID); err != nil {
		t.Fatal(err)
	}
	alice.next(10*time.Second, "the subscription closed for want of row-level security", func(m rtMessage) bool {
		return m.Event == "system" && strings.Contains(string(m.Payload), "no longer has row-level security")
	})
	alice.quiet(time.Second, "bob's row at alice without row-level security", isChange("INSERT", "bob after rls"))
	if _, err := app.Exec(ctx, `ALTER TABLE todos ENABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
}

func isBroadcast(event string) func(rtMessage) bool {
	return func(m rtMessage) bool {
		if m.Event != "broadcast" {
			return false
		}
		var b struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(m.Payload, &b)
		return b.Event == event
	}
}

// TestRealtimeBroadcast: broadcast and presence between clients on two
// xenibase-edge processes (relayed through the project's database), private
// channels decided by policies on xb_realtime.channel_access, persisted
// history, the HTTP broadcast, the change rate cap, the connection limit,
// and metering.
func TestRealtimeBroadcast(t *testing.T) {
	rp := newRealtimeProject(t, "rt-chat")
	ctx := context.Background()
	// Topics kept as history are read every few seconds: list it first.
	owner := rp.ownerConn(t)
	if _, err := owner.Exec(ctx, `INSERT INTO xb_realtime.persisted_topics (topic) VALUES ('vip')`); err != nil {
		t.Fatal(err)
	}
	owner.Close(ctx)
	ed2 := rp.e.StartEdge(func(c *edge.Config) { c.Name = "edge-test-2" })
	waitFor(t, 30*time.Second, "the second edge reaches the project", func() bool {
		code, _, _ := ed2.Do(rp.ref, "GET", "/data/v1/health", nil, "apikey", rp.pub)
		return code == 200
	})
	alice := connectRT(t, rp.ed, rp.ref, rp.pub)
	bob := connectRT(t, ed2, rp.ref, rp.pub)
	room := map[string]any{"broadcast": map[string]any{"self": false, "ack": true}, "presence": map[string]any{"key": "alice"}}
	if st, _ := alice.join("realtime:room1", room, rp.alice); st != "ok" {
		t.Fatal("alice joins room1")
	}
	room2 := map[string]any{"broadcast": map[string]any{"self": true}, "presence": map[string]any{"key": "bob"}}
	if st, _ := bob.join("realtime:room1", room2, rp.bob); st != "ok" {
		t.Fatal("bob joins room1")
	}

	// ---- Broadcast across processes ----------------------------------------------
	waitFor(t, 30*time.Second, "a broadcast crosses from one edge process to the other", func() bool {
		alice.send("realtime:room1", "broadcast", map[string]any{"type": "broadcast", "event": "ping", "payload": map[string]any{"n": 1}})
		select {
		case m := <-bob.in:
			return isBroadcast("ping")(m)
		case <-time.After(500 * time.Millisecond):
			return false
		}
	})
	ref := alice.send("realtime:room1", "broadcast", map[string]any{"type": "broadcast", "event": "cursor", "payload": map[string]any{"x": 3}})
	alice.next(5*time.Second, "the broadcast ack", func(m rtMessage) bool { return m.Event == "phx_reply" && m.Ref != nil && *m.Ref == ref })
	m := bob.next(5*time.Second, "alice's cursor at bob", isBroadcast("cursor"))
	if !strings.Contains(string(m.Payload), `"x":3`) {
		t.Fatalf("broadcast payload: %s", m.Payload)
	}
	alice.quiet(500*time.Millisecond, "alice's own broadcast (self is off)", isBroadcast("cursor"))
	bob.send("realtime:room1", "broadcast", map[string]any{"type": "broadcast", "event": "echo", "payload": map[string]any{}})
	bob.next(5*time.Second, "bob's own broadcast (self is on)", isBroadcast("echo"))
	alice.next(5*time.Second, "bob's broadcast at alice", isBroadcast("echo"))

	// ---- Presence across processes ----------------------------------------------
	alice.send("realtime:room1", "presence", map[string]any{"type": "presence", "event": "track", "payload": map[string]any{"status": "online"}})
	bob.next(10*time.Second, "alice's presence at bob", func(m rtMessage) bool {
		return m.Event == "presence_diff" && strings.Contains(string(m.Payload), `"alice"`) && strings.Contains(string(m.Payload), "online")
	})
	carol := connectRT(t, ed2, rp.ref, rp.pub)
	if st, _ := carol.join("realtime:room1", map[string]any{"presence": map[string]any{"key": "carol"}}, ""); st != "ok" {
		t.Fatal("carol joins")
	}
	carol.next(5*time.Second, "the presence state with alice", func(m rtMessage) bool {
		return m.Event == "presence_state" && strings.Contains(string(m.Payload), `"alice"`)
	})
	_ = alice.ws.Close(websocket.StatusNormalClosure, "")
	bob.next(10*time.Second, "alice leaving", func(m rtMessage) bool {
		return m.Event == "presence_diff" && strings.Contains(string(m.Payload), `"leaves":{"alice"`)
	})

	// ---- Private channels -----------------------------------------------------------
	app := rp.ownerConn(t)
	defer app.Close(ctx)
	for _, st := range []string{
		`CREATE TABLE members (room text NOT NULL, user_id uuid NOT NULL, PRIMARY KEY (room, user_id))`,
		fmt.Sprintf(`INSERT INTO members VALUES ('vip', '%s')`, rp.bobID),
		fmt.Sprintf(`GRANT SELECT ON members TO %q`, store.UserRole(rp.db)),
		fmt.Sprintf(`CREATE POLICY room_members ON xb_realtime.channel_access FOR SELECT TO %q
		  USING (EXISTS (SELECT 1 FROM members m WHERE m.room = topic AND m.user_id = xb_auth.uid()))`, store.UserRole(rp.db)),
	} {
		if _, err := app.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	private := map[string]any{"private": true, "broadcast": map[string]any{"ack": true}}
	alice = connectRT(t, rp.ed, rp.ref, rp.pub)
	if st, resp := alice.join("realtime:vip", private, rp.alice); st != "error" || !strings.Contains(string(resp), "not allowed") {
		t.Fatalf("a non-member joins a private channel: %s %s", st, resp)
	}
	if st, resp := bob.join("realtime:vip", private, rp.bob); st != "ok" {
		t.Fatalf("a member joins: %s %s", st, resp)
	}
	// Without an INSERT policy the member may read but not send.
	ref = bob.send("realtime:vip", "broadcast", map[string]any{"type": "broadcast", "event": "hi", "payload": map[string]any{}})
	bob.next(5*time.Second, "the refused send", func(m rtMessage) bool {
		return m.Event == "phx_reply" && m.Ref != nil && *m.Ref == ref && strings.Contains(string(m.Payload), "not allowed to send")
	})
	if _, err := app.Exec(ctx, fmt.Sprintf(`CREATE POLICY room_send ON xb_realtime.channel_access FOR INSERT TO %q
		WITH CHECK (EXISTS (SELECT 1 FROM members m WHERE m.room = topic AND m.user_id = xb_auth.uid()))`, store.UserRole(rp.db))); err != nil {
		t.Fatal(err)
	}
	if st, _ := bob.join("realtime:vip", private, rp.bob); st != "ok" {
		t.Fatal("rejoin")
	}
	ref = bob.send("realtime:vip", "broadcast", map[string]any{"type": "broadcast", "event": "secret", "payload": map[string]any{"msg": "for members"}})
	bob.next(5*time.Second, "the send's ack", func(m rtMessage) bool {
		return m.Event == "phx_reply" && m.Ref != nil && *m.Ref == ref && strings.Contains(string(m.Payload), `"ok"`)
	})
	// A public channel of the same name doesn't hear the private one.
	if st, _ := alice.join("realtime:vip", map[string]any{}, rp.alice); st != "ok" {
		t.Fatal("alice joins the public vip")
	}
	alice.quiet(time.Second, "a private broadcast on the public channel", isBroadcast("secret"))

	// ---- History and the HTTP broadcast ------------------------------------------
	code, _, body := rp.ed.Do(rp.ref, "GET", "/realtime/v1/history/vip?private=true", nil, "apikey", rp.pub, "Authorization", "Bearer "+rp.bob)
	if code != 200 || !strings.Contains(body, "for members") {
		t.Fatalf("history as a member: %d %s", code, body)
	}
	if code, _, body := rp.ed.Do(rp.ref, "GET", "/realtime/v1/history/vip?private=true", nil, "apikey", rp.pub, "Authorization", "Bearer "+rp.alice); code != 403 {
		t.Fatalf("history as a non-member: %d %s", code, body)
	}
	// The public channel's history, which anyone with the key reads, has
	// none of the private channel's.
	if code, _, body := rp.ed.Do(rp.ref, "GET", "/realtime/v1/history/vip", nil, "apikey", rp.pub); code != 200 || strings.Contains(body, "for members") {
		t.Fatalf("the public history of a private topic: %d %s", code, body)
	}
	if code, _, body := rp.ed.Do(rp.ref, "GET", "/realtime/v1/history/vip?private=true", nil, "apikey", rp.sec); code != 200 || !strings.Contains(body, "for members") {
		t.Fatalf("history with the secret key: %d %s", code, body)
	}
	if code, _, body := rp.ed.Do(rp.ref, "POST", "/realtime/v1/api/broadcast", strings.NewReader(`{"messages":[{"topic":"room1","event":"notice","payload":{"text":"from the server"}}]}`),
		"apikey", rp.pub, "Content-Type", "application/json"); code != 403 {
		t.Fatalf("HTTP broadcast with the publishable key: %d %s", code, body)
	}
	if code, _, body := rp.ed.Do(rp.ref, "POST", "/realtime/v1/api/broadcast", strings.NewReader(`{"messages":[{"topic":"room1","event":"notice","payload":{"text":"from the server"}}]}`),
		"apikey", rp.sec, "Content-Type", "application/json"); code != 202 {
		t.Fatalf("HTTP broadcast: %d %s", code, body)
	}
	bob.next(10*time.Second, "the server's broadcast on the other edge", isBroadcast("notice"))

	// ---- The change rate cap: past it, subscribers are told to resync --------
	todos := map[string]any{"postgres_changes": []map[string]any{{"event": "*", "schema": "public", "table": "todos"}}}
	if st, _ := carol.join("realtime:todos", todos, rp.bob); st != "ok" {
		t.Fatal("carol subscribes")
	}
	if _, err := app.Exec(ctx, `INSERT INTO todos (owner, body) SELECT $1, 'bulk ' || g FROM generate_series(1, 500) g`, rp.bobID); err != nil {
		t.Fatal(err)
	}
	carol.next(15*time.Second, "a resync past the change rate", func(m rtMessage) bool {
		return m.Event == "system" && strings.Contains(string(m.Payload), "resync")
	})

	// ---- The connection limit (per edge process; alice holds the first edge's one) --
	if _, err := rp.e.DB.Exec(ctx, `UPDATE project_services SET realtime_max_connections = 1 WHERE project_id = $1`, rp.pid); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the connection limit reaches the edge", func() bool {
		u := strings.Replace(rp.ed.URL, "http://", "ws://", 1) + "/realtime/v1/websocket?apikey=" + url.QueryEscape(rp.pub)
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ws, res, err := websocket.Dial(cctx, u, &websocket.DialOptions{Host: rp.ref + "." + testenv.EdgeDomain})
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		if err == nil {
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return false
		}
		return res != nil && res.StatusCode == http.StatusTooManyRequests
	})

	// ---- Metering --------------------------------------------------------------------
	for _, ed := range []*testenv.Edge{rp.ed, ed2} {
		ed.Flush(ctx)
	}
	var conns, msgs float64
	waitFor(t, 30*time.Second, "realtime usage is recorded", func() bool {
		for _, ed := range []*testenv.Edge{rp.ed, ed2} {
			ed.Flush(ctx)
		}
		_ = rp.e.DB.QueryRow(ctx, `SELECT
			coalesce(sum(quantity) FILTER (WHERE metric = 'realtime_connection_minutes'), 0)::float8,
			coalesce(sum(quantity) FILTER (WHERE metric = 'realtime_messages'), 0)::float8
			FROM usage_records WHERE project_id = $1`, rp.pid).Scan(&conns, &msgs)
		return conns > 0 && msgs > 0
	})
}
