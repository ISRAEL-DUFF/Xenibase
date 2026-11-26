package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestPausedProjectAnswersResuming is Taskiem's Q13: an API call that
// needs the database of a project asleep for inactivity answers 503
// project_resuming (or project_restoring) with a Retry-After, and wakes
// the project, so an integrator retries instead of giving up.
func TestPausedProjectAnswersResuming(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	c := e.CreateProject("Sleepy")
	pid := c.Project.Id.String()
	app := e.MustConnect(c.Connection.SessionUrl)
	if _, err := app.Exec(ctx, `CREATE TABLE orders (id serial PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	_ = app.Close(ctx)
	allowLocal(t, e, e.OrgID)
	rc := newReceiver(t)
	wh := createWebhook(t, e, pid, gen.WebhookRequest{Name: "taskiem-1", Tables: []string{"orders"}, Url: rc.URL,
		Events: []gen.WebhookRequestEvents{gen.WebhookRequestEventsINSERT}})
	token := e.CreateToken(map[string]any{"name": "taskiem", "org_id": e.OrgID, "scopes": []string{"read", "write"},
		"project_ids": []string{pid}})

	call := func(method, path string, body any) (int, http.Header, gen.Error) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, err := http.NewRequest(method, e.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var apiErr gen.Error
		_ = json.Unmarshal(raw, &apiErr)
		return res.StatusCode, res.Header, apiErr
	}

	for _, tc := range []struct{ lifecycle, code, retry string }{
		{"paused", "project_resuming", "10"},
		{"archived", "project_restoring", "60"},
	} {
		// Each call queues a wake, and a shared project's resume can finish
		// before the next call: let it, and put the project back to sleep.
		sleep := func() {
			t.Helper()
			awaitPay(t, "the wake", func() bool { return !busy(t, e, c.Project) })
			if _, err := e.DB.Exec(ctx, `UPDATE projects SET lifecycle = $2 WHERE id = $1`, c.Project.Id, tc.lifecycle); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range []struct {
			method, path string
			body         any
		}{
			{"POST", "/api/v1/projects/" + pid + "/sql", map[string]any{"query": "select 1", "query_id": uuid.NewString()}},
			{"PATCH", "/api/v1/projects/" + pid + "/webhooks/" + wh.Webhook.Id.String(), map[string]any{"enabled": true}},
		} {
			sleep()
			code, h, apiErr := call(r.method, r.path, r.body)
			if code != http.StatusServiceUnavailable || apiErr.Code != tc.code || h.Get("Retry-After") != tc.retry {
				t.Fatalf("%s %s on a %s project: %d %+v Retry-After=%q", r.method, r.path, tc.lifecycle,
					code, apiErr, h.Get("Retry-After"))
			}
		}
		// Reads that don't need the database still answer.
		sleep()
		if code, _, apiErr := call("GET", "/api/v1/projects/"+pid+"/webhooks", nil); code != http.StatusOK {
			t.Fatalf("listing webhooks on a %s project: %d %+v", tc.lifecycle, code, apiErr)
		}
		// And the calls queued the wake: at least one (an unarchive with no
		// backup fails at once, so a later call can queue another).
		var wakes int
		if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM operations WHERE project_id = $1 AND kind = $2`,
			c.Project.Id, map[string]string{"paused": "resume_project", "archived": "unarchive_project"}[tc.lifecycle]).Scan(&wakes); err != nil || wakes < 1 {
			t.Fatalf("wakes queued for a %s project: %d %v", tc.lifecycle, wakes, err)
		}
		// Let the wake finish, then reset for the next case.
		awaitPay(t, "the wake", func() bool { return !busy(t, e, c.Project) })
		if _, err := e.DB.Exec(ctx, `UPDATE projects SET lifecycle = 'active' WHERE id = $1`, c.Project.Id); err != nil {
			t.Fatal(err)
		}
	}
}
