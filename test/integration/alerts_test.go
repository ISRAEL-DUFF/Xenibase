package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/alerts"
	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// hook records webhook deliveries and checks their signatures.
type hook struct {
	t      *testing.T
	secret string
	fail   atomic.Bool
	mu     sync.Mutex
	got    []alerts.Payload
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sig := r.Header.Get("X-Xenibase-Signature")
	ts := ""
	for _, part := range strings.Split(sig, ",") {
		if v, ok := strings.CutPrefix(part, "t="); ok {
			ts = v
		}
	}
	var unix int64
	for _, c := range ts {
		unix = unix*10 + int64(c-'0')
	}
	if want := alerts.Sign(h.secret, time.Unix(unix, 0), body); sig != want || time.Since(time.Unix(unix, 0)) > time.Minute {
		h.t.Errorf("bad signature %q (want %q)", sig, want)
	}
	if h.fail.Load() {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		return
	}
	var p alerts.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		h.t.Errorf("payload: %v", err)
	}
	if r.Header.Get("X-Xenibase-Event") != p.Event {
		h.t.Errorf("event header %q, body %q", r.Header.Get("X-Xenibase-Event"), p.Event)
	}
	h.mu.Lock()
	h.got = append(h.got, p)
	h.mu.Unlock()
}

// take returns and forgets the deliveries so far.
func (h *hook) take() []alerts.Payload {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.got
	h.got = nil
	return out
}

func events(ps []alerts.Payload) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		out[p.Alert.Kind+"/"+p.Alert.Target.ID] = p.Event
	}
	return out
}

// TestAlerts covers spec §8.8: each trigger fires once, is delivered to the
// webhook (signed) and by email, and resolves when the condition clears;
// failed deliveries retry.
func TestAlerts(t *testing.T) {
	ghost, err := pooler.NewAdmin("ghost", "127.0.0.1:1", "xenibase", "x", "disable")
	if err != nil {
		t.Fatal(err)
	}
	e := testenv.Start(t, testenv.Options{ExtraPoolers: []*pooler.Admin{ghost}})
	ctx := context.Background()
	h := &hook{t: t, secret: "whsec-test-secret"}
	srv := httptest.NewServer(h)
	defer srv.Close()
	mail := testenv.StartSMTP(t)
	host, port := "127.0.0.1", 0
	for _, c := range mail.Addr[strings.LastIndex(mail.Addr, ":")+1:] {
		port = port*10 + int(c-'0')
	}

	// Settings: validation, then both channels; secrets never come back.
	for _, bad := range []map[string]any{
		{"webhook_url": "ftp://nope"},
		{"smtp": map[string]any{"host": host, "port": port, "from": "not an address", "to": []string{"ops@example.com"}}},
		{"smtp": map[string]any{"host": host, "port": port, "from": "xenibase@example.com", "to": []string{}}},
		{"smtp": map[string]any{"host": host, "port": port, "from": "xenibase@example.com", "to": []string{"ops@example.com"}, "tls": "ssl3"}},
	} {
		if code := e.Do("PUT", "/api/v1/settings/alerts", bad, nil); code != http.StatusBadRequest {
			t.Errorf("settings %v: %d", bad, code)
		}
	}
	var st gen.AlertSettings
	if code := e.Do("PUT", "/api/v1/settings/alerts", map[string]any{
		"webhook_url": srv.URL + "/hook", "webhook_secret": h.secret,
		"smtp": map[string]any{"host": host, "port": port, "tls": "none", "username": "mailer", "password": "smtp-pass",
			"from": "Xenibase <xenibase@example.com>", "to": []string{"ops@example.com", "oncall@example.com"}},
	}, &st); code != http.StatusOK || !st.HasWebhookSecret || st.Smtp == nil || !st.Smtp.HasPassword {
		t.Fatalf("save: %d %+v", code, st)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "smtp-pass") || strings.Contains(string(raw), h.secret) {
		t.Fatalf("secrets returned: %s", raw)
	}
	var stored []byte
	if err := e.DB.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = 'alerts'`).Scan(&stored); err != nil ||
		strings.Contains(string(stored), "smtp-pass") || strings.Contains(string(stored), h.secret) {
		t.Fatalf("secrets stored in the clear: %s %v", stored, err)
	}

	var res gen.AlertTestResult
	if code := e.Do("POST", "/api/v1/settings/alerts/test", nil, &res); code != http.StatusOK || len(res.Results) != 2 {
		t.Fatalf("test: %d %+v", code, res)
	}
	for _, r := range res.Results {
		if !r.Ok {
			t.Fatalf("test via %s: %s", r.Channel, deref(r.Error))
		}
	}
	if got := h.take(); len(got) != 1 || got[0].Event != alerts.EventTest {
		t.Fatalf("test webhook: %+v", got)
	}
	if m := mail.Mail(); len(m) != 1 || !strings.Contains(m[0].Data, "Subject: [Xenibase] Test alert") ||
		len(m[0].To) != 2 || m[0].From != "xenibase@example.com" || m[0].Auth != "|mailer|smtp-pass" {
		t.Fatalf("test mail: %+v", m)
	}

	// Set up every trigger.
	c := e.CreateProject("Watched")
	pid := c.Project.Id
	q := store.New(e.DB)
	insts, err := q.ListLiveSharedInstances(ctx)
	if err != nil || len(insts) == 0 {
		t.Fatalf("instances: %v %v", insts, err)
	}
	inst := insts[0]
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The test node behaves like one with an agent.
	exec(`UPDATE nodes SET agent_cert_fp = 'test', last_reachable_at = now(), capacity = '{"disk_path":"/data","disk_total_bytes":1000,"disk_free_bytes":100}' WHERE id = $1`, inst.NodeID)
	exec(`INSERT INTO operations (kind, project_id, status, error, finished_at) VALUES ('backup', $1, 'failed', 'S3 unreachable', now())`, pid)
	exec(`INSERT INTO operations (kind, project_id, status, error, finished_at) VALUES ('restore_test', $1, 'failed', 'row counts differ', now())`, pid)
	exec(`INSERT INTO operations (kind, status, error, finished_at) VALUES ('metadata_backup', 'failed', 'agent unreachable', now())`)
	exec(`INSERT INTO operations (kind, params, status, error, finished_at) VALUES ('isolation_check', jsonb_build_object('instance_id', $1::text), 'failed', '1 isolation finding(s)', now())`, inst.ID)
	exec(`UPDATE projects SET created_at = now() - interval '27 hours' WHERE id = $1`, pid)
	exec(`INSERT INTO metric_points VALUES ('project', $1, 'size_bytes', date_trunc('minute', now()), '1m', 2e9)`, pid) // warn is 1 GiB

	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		alerts.KindBackupFailed + "/" + pid.String():       alerts.EventFiring,
		alerts.KindBackupFailed + "/metadata":              alerts.EventFiring,
		alerts.KindRestoreTestFailed + "/" + pid.String():  alerts.EventFiring,
		alerts.KindBackupOverdue + "/" + pid.String():      alerts.EventFiring,
		alerts.KindProjectDisk + "/" + pid.String():        alerts.EventFiring,
		alerts.KindNodeDisk + "/" + inst.NodeID.String():   alerts.EventFiring,
		alerts.KindIsolationCheck + "/" + inst.ID.String(): alerts.EventFiring,
		alerts.KindPoolerDown + "/ghost":                   alerts.EventFiring,
	}
	got := events(h.take())
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("deliveries %v, want %v", got, want)
	}
	if n := len(mail.Mail()); n != 1+len(want) {
		t.Errorf("%d mails, want %d", n, 1+len(want))
	}
	var crit bool
	for _, m := range mail.Mail() {
		if strings.Contains(m.Data, "Subject: [Xenibase] CRITICAL: The ghost pooler at 127.0.0.1:1 is down") {
			crit = true
		}
	}
	if !crit {
		t.Error("no CRITICAL pooler mail")
	}

	// No repeats while conditions hold.
	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again := h.take(); len(again) != 0 {
		t.Fatalf("repeated notifications: %v", events(again))
	}
	var list gen.AlertList
	if code := e.Do("GET", "/api/v1/alerts?status=firing", nil, &list); code != http.StatusOK || list.Firing != int64(len(want)) || list.Critical != 5 {
		t.Fatalf("firing list: %d firing=%d critical=%d", code, list.Firing, list.Critical)
	}
	for _, a := range list.Items {
		if a.NotifiedAt == nil || a.Status != "firing" {
			t.Errorf("alert %s: %+v", a.Kind, a)
		}
	}

	// The node goes quiet: unreachable after two minutes (and its disk
	// alert is no longer judged).
	exec(`UPDATE nodes SET last_reachable_at = now() - interval '3 minutes' WHERE id = $1`, inst.NodeID)
	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got = events(h.take())
	if got[alerts.KindNodeUnreachable+"/"+inst.NodeID.String()] != alerts.EventFiring || got[alerts.KindNodeDisk+"/"+inst.NodeID.String()] != alerts.EventResolved {
		t.Fatalf("node quiet: %v", got)
	}

	// Everything clears: each alert resolves once.
	exec(`UPDATE nodes SET last_reachable_at = now(), capacity = '{"disk_total_bytes":1000,"disk_free_bytes":900}' WHERE id = $1`, inst.NodeID)
	for _, kind := range []string{"backup", "restore_test"} {
		exec(`INSERT INTO operations (kind, project_id, status, finished_at) VALUES ($1, $2, 'succeeded', now())`, kind, pid)
	}
	exec(`INSERT INTO operations (kind, status, finished_at) VALUES ('metadata_backup', 'succeeded', now())`)
	exec(`INSERT INTO operations (kind, params, status, finished_at) VALUES ('isolation_check', jsonb_build_object('instance_id', $1::text), 'succeeded', now())`, inst.ID)
	exec(`INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status) VALUES ($1, 'logical', 'k', now(), now(), 'succeeded')`, pid)
	exec(`UPDATE metric_points SET value = 1e6 WHERE scope_id = $1`, pid)

	// The webhook is down at first: the resolutions wait and retry.
	h.fail.Store(true)
	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE status = 'resolved' AND resolved_notified_at IS NULL AND delivery_error LIKE '%503%'`).Scan(&pending); err != nil || pending != 7 {
		t.Fatalf("undelivered resolutions: %d %v", pending, err)
	}
	h.take()
	h.fail.Store(false)
	exec(`UPDATE alerts SET delivery_lock = now() - interval '1 second'`) // the backoff has passed
	exec(`UPDATE alert_deliveries SET next_at = now() - interval '1 second' WHERE next_at IS NOT NULL`)
	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got = events(h.take())
	for k := range want {
		if k == alerts.KindPoolerDown+"/ghost" || k == alerts.KindNodeDisk+"/"+inst.NodeID.String() {
			continue // still down / resolved earlier
		}
		if got[k] != alerts.EventResolved {
			t.Errorf("%s after recovery: %q", k, got[k])
		}
	}
	if got[alerts.KindNodeUnreachable+"/"+inst.NodeID.String()] != alerts.EventResolved {
		t.Errorf("node unreachable not resolved: %v", got)
	}
	list = gen.AlertList{}
	e.Do("GET", "/api/v1/alerts?status=firing", nil, &list)
	if list.Firing != 1 || list.Items[0].Kind != alerts.KindPoolerDown {
		t.Fatalf("still firing: %+v", list)
	}

	// A condition that returns opens a new alert.
	exec(`INSERT INTO operations (kind, project_id, status, error, finished_at) VALUES ('backup', $1, 'failed', 'again', now())`, pid)
	if err := e.Alerts.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := events(h.take()); got[alerts.KindBackupFailed+"/"+pid.String()] != alerts.EventFiring {
		t.Fatalf("refire: %v", got)
	}
	var count int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'backup_failed' AND target_id = $1`, pid.String()).Scan(&count); err != nil || count != 2 {
		t.Fatalf("backup_failed alerts: %d %v", count, err)
	}
}
