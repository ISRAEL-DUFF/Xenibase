package statuspage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/test/testenv"
)

func TestAdvanceIncidentRules(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	steps := []struct {
		state string
		want  action
		fails int
		oks   int
	}{
		{statusapi.Operational, noAction, 0, 1},
		{statusapi.Down, noAction, 1, 0},
		{statusapi.Unknown, noAction, 0, 0}, // unknown resets the run
		{statusapi.Down, noAction, 1, 0},
		{statusapi.Degraded, noAction, 2, 0}, // degraded counts as failing
		{statusapi.Down, openIncident, 3, 0},
		{statusapi.Down, noAction, 4, 0}, // already open
		{statusapi.Operational, noAction, 0, 1},
		{statusapi.Down, noAction, 1, 0}, // a blip doesn't resolve
		{statusapi.Operational, noAction, 0, 1},
		{statusapi.Operational, resolveIncident, 0, 2},
	}
	var tr track
	for i, s := range steps {
		var act action
		tr, act = advance(tr, s.state, "", now.Add(time.Duration(i)*time.Minute), 3, 2)
		if act != s.want || tr.Fails != s.fails || tr.Oks != s.oks {
			t.Fatalf("step %d (%s): action %v fails %d oks %d, want %v %d %d", i, s.state, act, tr.Fails, tr.Oks, s.want, s.fails, s.oks)
		}
		switch act {
		case openIncident:
			tr.AutoIncident = "x"
		case resolveIncident:
			tr.AutoIncident = ""
		}
	}
	if !tr.Since.Equal(now.Add(9 * time.Minute)) {
		t.Fatalf("since %s: should be when it last changed state", tr.Since)
	}
}

func TestCombineAndDisplay(t *testing.T) {
	bad := errors.New("refused")
	for _, c := range []struct {
		errs []error
		want string
	}{
		{[]error{nil}, statusapi.Operational},
		{[]error{bad}, statusapi.Down},
		{[]error{nil, bad}, statusapi.Degraded},
		{[]error{bad, bad}, statusapi.Down},
	} {
		var rs []probeResult
		for i, e := range c.errs {
			rs = append(rs, probeResult{Name: "p" + strconv.Itoa(i), Err: e})
		}
		if got, _ := combine(rs); got != c.want {
			t.Errorf("combine %v = %s, want %s", c.errs, got, c.want)
		}
	}
	open := []statusapi.Incident{
		{ID: "a", Components: []string{"api"}, Severity: "minor"},
		{ID: "b", Components: []string{"db"}, Severity: "critical"},
		{ID: "c", Components: []string{"web"}, Severity: "critical", Auto: true}, // auto: measured already says it
	}
	for _, c := range []struct{ measured, id, want string }{
		{statusapi.Operational, "api", statusapi.Degraded},
		{statusapi.Down, "api", statusapi.Down},
		{statusapi.Operational, "db", statusapi.Down},
		{statusapi.Operational, "web", statusapi.Operational},
		{statusapi.Unknown, "other", statusapi.Unknown},
	} {
		if got := displayState(c.measured, c.id, open); got != c.want {
			t.Errorf("display %s/%s = %s, want %s", c.measured, c.id, got, c.want)
		}
	}
	for _, c := range []struct {
		states []string
		want   string
	}{
		{[]string{"operational", "operational"}, statusapi.Operational},
		{[]string{"operational", "unknown"}, statusapi.Unknown},
		{[]string{"unknown", "degraded"}, statusapi.Degraded},
		{[]string{"degraded", "down"}, statusapi.Down},
	} {
		if got, _ := overall(c.states); got != c.want {
			t.Errorf("overall %v = %s, want %s", c.states, got, c.want)
		}
	}
}

func TestHeartbeatState(t *testing.T) {
	now := time.Now()
	if s, _ := heartbeatState(heartbeat{}, false, now, time.Minute); s != statusapi.Unknown {
		t.Fatal(s)
	}
	if s, _ := heartbeatState(heartbeat{State: statusapi.Down, At: now.Add(-30 * time.Second)}, true, now, time.Minute); s != statusapi.Down {
		t.Fatal(s)
	}
	if s, _ := heartbeatState(heartbeat{State: statusapi.Operational, At: now.Add(-2 * time.Minute)}, true, now, time.Minute); s != statusapi.Unknown {
		t.Fatalf("a stale heartbeat must be unknown, never operational: %s", s)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "dsn")
	if err := os.WriteFile(dsn, []byte("postgres://probe:x@203.0.113.5:6543/probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := `public_url = "https://status.example.com/"
push_secret = "0123456789abcdef0123456789abcdef"
interval = "30s"

[[component]]
id = "edge-pooler"
name = "Edge pooler"
region = "eu-central"
  [[component.probe]]
  name = "transaction port"
  kind = "postgres"
  dsn_file = "` + dsn + `"

[[component]]
id = "backups"
heartbeat = true
`
	p := filepath.Join(dir, "status.toml")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://status.example.com" || c.Interval.Duration != 30*time.Second || c.FailAfter != 3 ||
		c.Components[0].Probes[0].DSN != "postgres://probe:x@203.0.113.5:6543/probe" || c.Components[0].Probes[0].Query != "SELECT 1" ||
		c.Components[1].Name != "backups" {
		t.Fatalf("config: %+v", c)
	}
	for name, bad := range map[string]string{
		"unknown key":   good + "\nbogus = 1\n",
		"short secret":  strings.Replace(good, "0123456789abcdef0123456789abcdef", "short", 1),
		"neither":       good + "\n[[component]]\nid = \"nothing\"\n",
		"heartbeat_id":  good + "\n[[component]]\nid = \"x\"\nheartbeat_id = \"y\"\n  [[component.probe]]\n  kind = \"tcp\"\n  addr = \"x:1\"\n",
		"no public url": strings.Replace(good, `public_url = "https://status.example.com/"`, "", 1),
		"bad kind":      strings.Replace(good, `kind = "postgres"`, `kind = "ping"`, 1),
		"duplicate id":  strings.Replace(good, `id = "backups"`, `id = "edge-pooler"`, 1),
	} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeProbes fails the probes named in down.
type fakeProbes struct {
	mu   sync.Mutex
	down map[string]bool
}

func (f *fakeProbes) set(name string, down bool) {
	f.mu.Lock()
	f.down[name] = down
	f.mu.Unlock()
}

func (f *fakeProbes) probe(_ context.Context, p Probe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[p.Name] {
		return errors.New("connection refused")
	}
	return nil
}

const secret = "0123456789abcdef0123456789abcdef"

func newTestService(t *testing.T, smtpAddr string) (*Service, *fakeProbes, *time.Time, *httptest.Server) {
	t.Helper()
	cfg := &Config{
		Data: filepath.Join(t.TempDir(), "status.db"), PublicURL: "https://status.example.com", PushSecret: secret,
		Components: []Component{
			{ID: "dashboard", Name: "Dashboard", Region: "eu-central", Probes: []Probe{{Name: "healthz", Kind: "http", URL: "https://x/healthz"}}},
			{ID: "edge-pooler", Name: "Edge pooler", Region: "eu-central", Probes: []Probe{
				{Name: "session", Kind: "tcp", Addr: "x:5432"}, {Name: "transaction", Kind: "tcp", Addr: "x:6543"}}},
			{ID: "backups", Name: "Backups", Region: "eu-central", Heartbeat: true},
		},
	}
	if smtpAddr != "" {
		host, port, _ := net.SplitHostPort(smtpAddr)
		p, _ := strconv.Atoi(port)
		cfg.SMTP = &SMTP{Host: host, Port: p, From: "status@example.com", TLS: "none"}
	}
	if err := cfg.resolve(); err != nil {
		t.Fatal(err)
	}
	svc, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	// Far from the real clock, so a push signed with it is refused.
	now := time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	fp := &fakeProbes{down: map[string]bool{}}
	svc.Probe = fp.probe
	ts := httptest.NewServer(svc.Handler())
	t.Cleanup(ts.Close)
	return svc, fp, &now, ts
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

type apiStatus struct {
	Status     string `json:"status"`
	Components []struct {
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Uptime *float64 `json:"uptime_percent"`
		Days   []struct {
			Day  string `json:"day"`
			Up   int    `json:"up_minutes"`
			Down int    `json:"down_minutes"`
		} `json:"days"`
	} `json:"components"`
	Active []statusapi.Incident `json:"active_incidents"`
	Recent []statusapi.Incident `json:"recent_incidents"`
	// Upcoming is announced maintenance not yet started (V3.1 §4.1).
	Upcoming []statusapi.Incident `json:"upcoming_maintenance"`
}

func (a apiStatus) component(id string) (string, *float64) {
	for _, c := range a.Components {
		if c.ID == id {
			return c.Status, c.Uptime
		}
	}
	return "", nil
}

func TestServiceEndToEnd(t *testing.T) {
	smtp := testenv.StartSMTP(t)
	svc, fp, now, ts := newTestService(t, smtp.Addr)
	ctx := context.Background()
	tick := func() {
		t.Helper()
		*now = now.Add(time.Minute)
		if err := svc.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	client := &statusapi.Client{URL: ts.URL, Secret: secret}

	// Subscribe (double opt-in) before anything happens.
	form := url.Values{"email": {"Ops@Example.com"}}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.PostForm(ts.URL+"/subscribe", form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "check-email") {
		t.Fatalf("subscribe: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	if err := svc.DeliverMail(ctx); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`/subscribe/confirm\?token=([0-9a-f]+)`).FindStringSubmatch(lastMail(t, smtp, "ops@example.com"))
	if m == nil {
		t.Fatal("no confirmation link")
	}
	// Following the link alone confirms nothing (mail scanners prefetch).
	if code := getCode(t, ts.URL+"/subscribe/confirm?token="+m[1]); code != http.StatusOK {
		t.Fatalf("confirm page: %d", code)
	}
	if n := svc.confirmedCount(t); n != 0 {
		t.Fatalf("confirmed by a GET: %d", n)
	}
	if code := postCode(t, ts.URL+"/subscribe/confirm", url.Values{"token": {m[1]}}); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	if n := svc.confirmedCount(t); n != 1 {
		t.Fatalf("confirmed subscribers: %d", n)
	}
	if code := postCode(t, ts.URL+"/subscribe/confirm", url.Values{"token": {m[1]}}); code != http.StatusBadRequest {
		t.Fatalf("a used confirmation link: %d", code)
	}

	// Healthy, with a heartbeat for backups.
	if err := client.Heartbeat(ctx, statusapi.Heartbeat{SentAt: *now, Components: []statusapi.ComponentState{{ID: "backups", Status: "operational"}}}); err == nil {
		t.Fatal("heartbeat signed with the real clock was accepted against a test clock far away")
	}
	push(t, svc, ts, "POST", statusapi.PathHeartbeat, statusapi.Heartbeat{SentAt: *now, Components: []statusapi.ComponentState{{ID: "backups", Status: "operational"}}})
	tick()
	var st apiStatus
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if st.Status != statusapi.Operational {
		t.Fatalf("overall %s: %+v", st.Status, st)
	}

	// One pooler port down: degraded. Both: down. Three failing checks in
	// a row open an incident by themselves.
	fp.set("transaction", true)
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if s, _ := st.component("edge-pooler"); s != statusapi.Degraded {
		t.Fatalf("one port down: %s", s)
	}
	fp.set("session", true)
	tick()
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if s, _ := st.component("edge-pooler"); s != statusapi.Down || len(st.Active) != 1 || !st.Active[0].Auto || st.Active[0].Severity != "major" {
		t.Fatalf("after 3 failing checks: %s, active %+v", s, st.Active)
	}
	inc := st.Active[0]
	// xenibase-server can't take over an automatic incident.
	inc.Updates = append(inc.Updates, statusapi.IncidentUpdate{ID: "x", Status: "identified", Body: "b", PostedAt: *now})
	if code := push(t, svc, ts, "PUT", statusapi.PathIncidents+inc.ID, inc); code != http.StatusConflict {
		t.Fatalf("overwrite an automatic incident: %d", code)
	}

	// Backups' heartbeat goes stale: unknown, never operational.
	*now = now.Add(5 * time.Minute)
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if s, _ := st.component("backups"); s != statusapi.Unknown {
		t.Fatalf("stale heartbeat: %s", s)
	}

	// Recovery: two good checks resolve the incident.
	fp.set("session", false)
	fp.set("transaction", false)
	tick()
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if len(st.Active) != 0 || len(st.Recent) != 1 || st.Recent[0].ResolvedAt == nil {
		t.Fatalf("after recovery: active %+v recent %+v", st.Active, st.Recent)
	}
	if _, up := st.component("edge-pooler"); up == nil || *up >= 100 || *up <= 0 {
		t.Fatalf("edge pooler uptime %v", up)
	}

	// A manual incident from xenibase-server, then an update to it.
	manual := statusapi.Incident{ID: "inc-1", Title: "Backups delayed", Components: []string{"backups"}, Severity: "minor", Status: "investigating",
		StartedAt: *now, Updates: []statusapi.IncidentUpdate{{ID: "1", Status: "investigating", Body: "Backups are running late.", PostedAt: *now}}}
	if code := push(t, svc, ts, "PUT", statusapi.PathIncidents+"inc-1", manual); code != http.StatusNoContent {
		t.Fatalf("push incident: %d", code)
	}
	push(t, svc, ts, "POST", statusapi.PathHeartbeat, statusapi.Heartbeat{SentAt: *now, Components: []statusapi.ComponentState{{ID: "backups", Status: "operational"}}})
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if s, _ := st.component("backups"); s != statusapi.Degraded {
		t.Fatalf("an open minor incident shows the component degraded: %s", s)
	}
	resolvedAt := now.Add(time.Minute)
	manual.Status, manual.ResolvedAt = "resolved", &resolvedAt
	manual.Updates = append(manual.Updates, statusapi.IncidentUpdate{ID: "2", Status: "resolved", Body: "Caught up.", PostedAt: resolvedAt})
	if code := push(t, svc, ts, "PUT", statusapi.PathIncidents+"inc-1", manual); code != http.StatusNoContent {
		t.Fatalf("push incident update: %d", code)
	}
	// Announced maintenance (V3.1 §4.1): upcoming until its window,
	// and the components it names stay as they are.
	soon := now.Add(72 * time.Hour)
	if code := push(t, svc, ts, "PUT", statusapi.PathIncidents+"maint-1", statusapi.Incident{ID: "maint-1", Title: "Kernel updates",
		Components: []string{"backups"}, Severity: "maintenance", Status: "identified", StartedAt: soon,
		Updates: []statusapi.IncidentUpdate{{ID: "1", Status: "identified", Body: "Scheduled for later.", PostedAt: *now}}}); code != http.StatusNoContent {
		t.Fatalf("push maintenance: %d", code)
	}
	tick()
	getJSON(t, ts.URL+"/api/v1/status", &st)
	if len(st.Upcoming) != 1 || st.Upcoming[0].ID != "maint-1" || len(st.Active) != 0 {
		t.Fatalf("upcoming maintenance: upcoming %+v active %+v", st.Upcoming, st.Active)
	}
	if s, _ := st.component("backups"); s == statusapi.Degraded {
		t.Fatalf("upcoming maintenance degraded its component")
	}
	if b := getBody(t, ts.URL+"/"); !strings.Contains(b, "data-upcoming") || !strings.Contains(b, "Kernel updates") {
		t.Fatalf("no upcoming maintenance on the page:\n%s", b)
	}
	// Replaying the same push notifies nobody again.
	push(t, svc, ts, "PUT", statusapi.PathIncidents+"inc-1", manual)
	if code := push(t, svc, ts, "PUT", statusapi.PathIncidents+"inc-2", statusapi.Incident{ID: "inc-2", Title: "x", Components: []string{"nope"},
		Severity: "minor", Status: "investigating", StartedAt: *now}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown component: %d", code)
	}

	// Bad signatures are refused.
	req, _ := http.NewRequest("POST", ts.URL+statusapi.PathHeartbeat, strings.NewReader(`{"components":[]}`))
	req.Header.Set("Xenibase-Signature", "t=1,v1=00")
	if code := doCode(t, req); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}

	// Subscribers got: opened, resolved (auto), and both manual updates.
	if err := svc.DeliverMail(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"have failed 3 times in a row", "resolved automatically", "Backups are running late.", "Caught up."} {
		if n := smtp.Count("ops@example.com", want); n != 1 {
			t.Errorf("emails containing %q: %d, want 1", want, n)
		}
	}

	// The HTML page, an incident page, and the feed.
	for path, want := range map[string]string{
		"/":                    "Edge pooler",
		"/incidents/inc-1":     "Caught up.",
		"/feed.rss":            "<title>Backups delayed — Resolved</title>",
		"/incidents/no-such":   "There is no such incident",
		"/api/v1/incidents/x9": "no such incident",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: no %q in:\n%s", path, want, b)
		}
		if strings.Contains(string(b), "<script") {
			t.Errorf("%s has a script", path)
		}
	}

	// Unsubscribe from the link in an email.
	m = regexp.MustCompile(`/unsubscribe\?token=([0-9a-f]+)`).FindStringSubmatch(lastMail(t, smtp, "ops@example.com"))
	if m == nil {
		t.Fatal("no unsubscribe link")
	}
	if code := postCode(t, ts.URL+"/unsubscribe", url.Values{"token": {m[1]}}); code != http.StatusOK {
		t.Fatalf("unsubscribe: %d", code)
	}
	if n := svc.confirmedCount(t); n != 0 {
		t.Fatalf("still subscribed: %d", n)
	}
}

func TestSubscribeRateLimit(t *testing.T) {
	smtp := testenv.StartSMTP(t)
	_, _, _, ts := newTestService(t, smtp.Addr)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last string
	for i := range 7 {
		resp, err := noRedirect.PostForm(ts.URL+"/subscribe", url.Values{"email": {"a" + strconv.Itoa(i) + "@example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		last = resp.Header.Get("Location")
	}
	if !strings.Contains(last, "m=limit") {
		t.Fatalf("7 requests in a row: %s", last)
	}
}

func push(t *testing.T, svc *Service, ts *httptest.Server, method, path string, v any) int {
	t.Helper()
	// Signed with the service's clock.
	body, _ := json.Marshal(v)
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(string(body)))
	req.Header.Set("Xenibase-Signature", statusapi.SignPush(secret, svc.Now(), method, path, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func lastMail(t *testing.T, s *testenv.SMTPServer, to string) string {
	t.Helper()
	ms := s.Mail()
	for i := len(ms) - 1; i >= 0; i-- {
		for _, r := range ms[i].To {
			if strings.EqualFold(r, to) {
				return strings.ReplaceAll(ms[i].Data, "=\r\n", "")
			}
		}
	}
	t.Fatalf("no mail to %s", to)
	return ""
}

func (s *Service) confirmedCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.st.db.QueryRow(`SELECT count(*) FROM subscribers WHERE confirmed_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func getCode(t *testing.T, u string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	return doCode(t, req)
}

func postCode(t *testing.T, u string, form url.Values) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doCode(t, req)
}

// doCode returns a response's status code and closes its body.
func doCode(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func getBody(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestHeartbeatStatesByRegion(t *testing.T) {
	s := &Service{cfg: &Config{Components: []Component{
		{ID: "backups", Region: "eu-central", Heartbeat: true, HeartbeatID: "backups"},
		{ID: "api-eu", Region: "eu-central", Heartbeat: true, HeartbeatID: "backend-services"},
		{ID: "api-lagos", Region: "lagos", Heartbeat: true, HeartbeatID: "backend-services"},
		{ID: "api-acc", Region: "accra", Heartbeat: true, HeartbeatID: "backend-services"},
	}}}
	got := s.heartbeatStates(statusapi.Heartbeat{Components: []statusapi.ComponentState{
		{ID: "backups", Status: statusapi.Degraded},
		{ID: "backend-services", Region: "eu-central", Status: statusapi.Operational},
		{ID: "backend-services", Region: "lagos", Status: statusapi.Down},
	}})
	if got["backups"].Status != statusapi.Degraded || got["api-eu"].Status != statusapi.Operational || got["api-lagos"].Status != statusapi.Down {
		t.Fatalf("states: %+v", got)
	}
	if _, ok := got["api-acc"]; ok {
		t.Fatalf("a region with no report got one: %+v", got["api-acc"])
	}
}

func TestConcerns(t *testing.T) {
	in := statusapi.Incident{Components: []string{"shared-tier"}, Region: "lagos"}
	for _, c := range []struct {
		comps, regions []string
		want           bool
	}{
		{[]string{"shared-tier"}, []string{"lagos"}, true},
		{[]string{"shared-tier"}, nil, true},
		{[]string{"shared-tier"}, []string{"eu-central"}, false},
		{[]string{"dedicated"}, []string{"lagos"}, false},
	} {
		if got := concerns(in, c.comps, c.regions); got != c.want {
			t.Errorf("concerns(%v, %v) = %v", c.comps, c.regions, got)
		}
	}
	if !concerns(statusapi.Incident{Components: []string{"dedicated"}}, []string{"dedicated"}, []string{"eu-central"}) {
		t.Error("an incident with no region concerns every region")
	}
}
