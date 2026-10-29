package nodes_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/agentsvc"
	"github.com/israel-duff/xenibase/internal/backupfmt"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	db    *pgxpool.Pool
	svc   *nodes.Service
	reg   *httptest.Server
	node  store.Node
	token string
}

func setup(t *testing.T, bootstrap string) *env {
	t.Helper()
	db := storetest.New(t)
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	ca, err := agentca.LoadOrCreate(context.Background(), db, kr)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := nodes.NewService(db, ca, bootstrap, quiet)
	if err != nil {
		t.Fatal(err)
	}
	n, err := store.New(db).UpsertNode(context.Background(), store.UpsertNodeParams{Name: "local", PrivateAddr: "127.0.0.1", Role: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc.NewToken(context.Background(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentapi.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, err := svc.Register(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(reg.Close)
	return &env{db: db, svc: svc, reg: reg, node: n, token: tok}
}

// startAgent registers an agent and serves it on a free port.
func (e *env) startAgent(t *testing.T, token, node string) *agentsvc.State {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	st, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: token, Node: node, AdvertiseHost: "127.0.0.1", AdvertisePort: port, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := agentsvc.New(agentsvc.Config{Version: "test", NodeID: st.NodeID, PGBinDir: os.Getenv("XENIBASE_TEST_PG_BIN")}, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = svc.Serve(ctx, ln, agentsvc.TLSConfig(st.Cert, st.CA)) }()
	t.Cleanup(cancel)
	return st
}

func TestRegisterAndHealth(t *testing.T) {
	e := setup(t, "")
	st := e.startAgent(t, e.token, "")
	if st.NodeID != e.node.ID.String() {
		t.Fatalf("registered as %s", st.NodeID)
	}
	a, err := e.svc.ForNode(context.Background(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.Health(context.Background())
	if err != nil || h.Version != "test" || h.NodeID != e.node.ID.String() {
		t.Fatalf("health: %+v %v", h, err)
	}
	m, err := a.Metrics(context.Background())
	if err != nil || m.CPUs < 1 || m.MemTotalBytes == 0 || m.DiskTotalBytes == 0 {
		t.Fatalf("metrics: %+v %v", m, err)
	}
	if s := e.svc.Check(context.Background(), a.Node); !s.Reachable {
		t.Fatalf("check: %+v", s)
	}

	// The token is single-use.
	if _, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: e.token, AdvertiseHost: "127.0.0.1", AdvertisePort: 1,
	}); err == nil || !strings.Contains(err.Error(), "invalid or has expired") {
		t.Fatalf("token reuse: %v", err)
	}
}

// TestClientCertRenewed: a server whose client certificate has run out
// (it was issued 40 days ago) still reaches its agents, because the
// handshake gets a fresh one.
func TestClientCertRenewed(t *testing.T) {
	e := setup(t, "")
	e.startAgent(t, e.token, "")
	if err := nodes.SetClientCertIssuedAt(e.svc, time.Now().Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.ForNode(context.Background(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health with an expired client certificate: %v", err)
	}
	if left := time.Until(e.svc.ClientCertExpiry()); left < 29*24*time.Hour {
		t.Fatalf("client certificate expires in %s", left)
	}
}

func TestPinnedFingerprint(t *testing.T) {
	e := setup(t, "")
	e.startAgent(t, e.token, "")
	// Swap the pin: the server must refuse the (otherwise valid) agent.
	_, err := e.db.Exec(context.Background(), `UPDATE nodes SET agent_cert_fp = 'sha256:00' WHERE id = $1`, e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := e.svc.ForNode(context.Background(), e.node.ID)
	if _, err := a.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned fingerprint") {
		t.Fatalf("expected pin failure, got %v", err)
	}
}

func TestBootstrapToken(t *testing.T) {
	e := setup(t, "bootstrap-secret")
	st := e.startAgent(t, "bootstrap-secret", "local")
	if st.NodeID != e.node.ID.String() {
		t.Fatal("bootstrap registered the wrong node")
	}
	// Only once: the node now has an agent.
	if _, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: "bootstrap-secret", Node: "local", AdvertiseHost: "x", AdvertisePort: 1,
	}); err == nil {
		t.Fatal("bootstrap token re-registered a node")
	}
	if _, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: "wrong", Node: "local", AdvertiseHost: "x", AdvertisePort: 1,
	}); err == nil {
		t.Fatal("wrong bootstrap token accepted")
	}
}

// TestDumpRestoreCopy runs real pg_dump/pg_restore. It needs
// XENIBASE_TEST_PG_BIN with tools matching XENIBASE_TEST_DATABASE_URL's server.
func TestDumpRestoreCopy(t *testing.T) {
	if os.Getenv("XENIBASE_TEST_PG_BIN") == "" {
		t.Skip("XENIBASE_TEST_PG_BIN not set")
	}
	e := setup(t, "")
	e.startAgent(t, e.token, "")
	a, _ := e.svc.ForNode(context.Background(), e.node.ID)
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, os.Getenv("XENIBASE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	src, dst, cpy := "agent_src_"+suffix(), "agent_dst_"+suffix(), "agent_cpy_"+suffix()
	for _, db := range []string{src, dst, cpy} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)") })
	}
	cfg := admin.Config()
	conn := func(db string) agentapi.PGConn {
		return agentapi.PGConn{Host: cfg.Host, Port: int(cfg.Port), User: cfg.User, Password: cfg.Password, SSLMode: "disable", Database: db}
	}
	sc := connectDB(t, src)
	_, err = sc.Exec(ctx, `CREATE TABLE t (id int PRIMARY KEY, v text); INSERT INTO t SELECT g, md5(g::text) FROM generate_series(1, 5000) g;
		CREATE SCHEMA other; CREATE TABLE other.skip (x int); INSERT INTO other.skip VALUES (1);`)
	_ = sc.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}

	fake, err := storage.NewFake("bk")
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	target := fake.Target("bk")
	backupKey := make([]byte, 32)
	fileKey, wrapped, _ := backupfmt.NewFileKey(backupKey)

	res, err := a.Dump(ctx, agentapi.DumpRequest{PG: conn(src), Upload: agentapi.Upload{
		Storage: target, ObjectKey: "t/1.dump.enc", FileKey: fileKey, WrappedKey: wrapped,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.SizeBytes == 0 || len(res.SHA256) != 64 || res.DumpBytes == 0 {
		t.Fatalf("dump result %+v", res)
	}
	// The object is encrypted, and opens with the backup key alone.
	cl, _ := storage.New(target)
	rc, _ := cl.Download(ctx, "t/1.dump.enc")
	head := make([]byte, 8)
	_, _ = io.ReadFull(rc, head)
	_ = rc.Close()
	if string(head) != "PGDKBK1\n" {
		t.Fatalf("object header %q", head)
	}

	if _, err := a.Restore(ctx, agentapi.RestoreRequest{
		Download: agentapi.Download{Storage: target, ObjectKey: "t/1.dump.enc", FileKey: fileKey}, PG: conn(dst),
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, dst, "t"); n != 5000 {
		t.Fatalf("restored %d rows", n)
	}

	// A wrong file key fails cleanly.
	if _, err := a.Restore(ctx, agentapi.RestoreRequest{
		Download: agentapi.Download{Storage: target, ObjectKey: "t/1.dump.enc", FileKey: make([]byte, 32)}, PG: conn(dst),
	}); err == nil {
		t.Fatal("restore with the wrong key succeeded")
	}

	// Copy only schema public. An explicit -n public dumps CREATE SCHEMA
	// public, so the import flow drops the empty target schema first.
	tc := connectDB(t, cpy)
	_, _ = tc.Exec(ctx, "DROP SCHEMA public")
	_ = tc.Close(ctx)
	if _, err := a.Copy(ctx, agentapi.CopyRequest{
		Source: conn(src), Dump: agentapi.DumpOptions{Schemas: []string{"public"}, NoOwner: true, NoACL: true},
		Target: conn(cpy),
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, cpy, "t"); n != 5000 {
		t.Fatalf("copied %d rows", n)
	}
	c := connectDB(t, cpy)
	var exists bool
	_ = c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'other')`).Scan(&exists)
	_ = c.Close(ctx)
	if exists {
		t.Fatal("copy included an unselected schema")
	}
}

func connectDB(t *testing.T, db string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("XENIBASE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func count(t *testing.T, db, table string) int {
	t.Helper()
	c := connectDB(t, db)
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func suffix() string { return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") }
