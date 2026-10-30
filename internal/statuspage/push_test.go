package statuspage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/signature"
	"github.com/israel-duff/xenibase/internal/statusapi"
)

func signedRequest(t *testing.T, method, url, header string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(signature.Header, header)
	return req
}

// A push's signature binds its method and path: a signed heartbeat
// replayed to the managed subscribers or SLA targets is refused (it would
// decode as an empty list and delete them all), and a body-only (v1)
// signature is refused.
func TestPushSignatureBindsEndpoint(t *testing.T) {
	svc, _, now, ts := newTestService(t, "")
	ctx := context.Background()
	if err := svc.PutManaged(ctx, statusapi.ManagedSubscribers{Subscribers: []statusapi.ManagedSubscriber{
		{Email: "owner@example.com", Components: []string{"backups"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.st.setSLATargets(ctx, []statusapi.SLATarget{{ID: "p1", DSN: "postgres://x@y/z"}}); err != nil {
		t.Fatal(err)
	}
	hb, _ := json.Marshal(statusapi.Heartbeat{SentAt: *now, Components: []statusapi.ComponentState{{ID: "backups", Status: "operational"}}})
	sig := statusapi.SignPush(secret, *now, http.MethodPost, statusapi.PathHeartbeat, hb)
	for _, path := range []string{statusapi.PathManagedSubscribers, statusapi.PathSLATargets} {
		if code := doCode(t, signedRequest(t, http.MethodPut, ts.URL+path, sig, hb)); code != http.StatusUnauthorized {
			t.Errorf("a heartbeat's signature replayed to PUT %s: %d", path, code)
		}
	}
	if list, err := svc.Managed(ctx); err != nil || len(list) != 1 {
		t.Fatalf("managed subscribers after the replay: %v %v", list, err)
	}
	if list, err := svc.st.slaTargets(ctx); err != nil || len(list) != 1 {
		t.Fatalf("SLA targets after the replay: %v %v", list, err)
	}
	// The body-only v1 signature alone is not enough.
	if code := doCode(t, signedRequest(t, http.MethodPost, ts.URL+statusapi.PathHeartbeat, signature.Sign(secret, *now, hb), hb)); code != http.StatusUnauthorized {
		t.Errorf("a v1-only signature: %d", code)
	}
	// The real one is accepted.
	if code := doCode(t, signedRequest(t, http.MethodPost, ts.URL+statusapi.PathHeartbeat, sig, hb)); code != http.StatusNoContent {
		t.Errorf("the heartbeat itself: %d", code)
	}
}

// A heartbeat sent before the newest one accepted is refused: replayed
// within the signature's five minutes, it would overwrite newer states.
func TestOlderHeartbeatRefused(t *testing.T) {
	svc, _, now, ts := newTestService(t, "")
	send := func(sentAt time.Time, state string) int {
		t.Helper()
		body, _ := json.Marshal(statusapi.Heartbeat{SentAt: sentAt, Components: []statusapi.ComponentState{{ID: "backups", Status: state}}})
		return doCode(t, signedRequest(t, http.MethodPost, ts.URL+statusapi.PathHeartbeat,
			statusapi.SignPush(secret, *now, http.MethodPost, statusapi.PathHeartbeat, body), body))
	}
	if code := send(now.Add(-2*time.Minute), statusapi.Down); code != http.StatusNoContent {
		t.Fatalf("first heartbeat: %d", code)
	}
	if code := send(now.Add(-time.Minute), statusapi.Operational); code != http.StatusNoContent {
		t.Fatalf("newer heartbeat: %d", code)
	}
	if code := send(now.Add(-2*time.Minute), statusapi.Down); code != http.StatusConflict {
		t.Fatalf("replayed older heartbeat: %d", code)
	}
	hbs, err := svc.st.heartbeats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hbs["backups"].State != statusapi.Operational {
		t.Fatalf("backups %s after the replay, want operational", hbs["backups"].State)
	}
}
