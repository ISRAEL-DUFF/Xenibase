package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestDedicatedConnectionLimit is Taskiem's 100 to 150 connections: a
// dedicated instance's max_connections grows with its memory, a project's
// connection limit goes up to it less Xenibase's reserve and no further,
// and a resize to less memory lowers both.
func TestDedicatedConnectionLimit(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "dedicated")
	ctx := context.Background()

	tier, profile, vol := gen.ProjectTierDedicated, "medium", 5
	var c gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: "Taskiem", Tier: &tier, Profile: &profile, VolumeGb: &vol}, &c); code != http.StatusAccepted {
		t.Fatalf("create: %d", code)
	}
	if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	pid := c.Project.Id.String()
	p, err := store.New(e.DB).GetProject(ctx, c.Project.Id)
	if err != nil {
		t.Fatal(err)
	}
	show := func() string {
		t.Helper()
		conn, err := e.Service.AdminConn(ctx, p.InstanceID, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var v string
		if err := conn.QueryRow(ctx, `SHOW max_connections`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	project := func() gen.Project {
		t.Helper()
		var got gen.Project
		if code := e.Do("GET", "/api/v1/projects/"+pid, nil, &got); code != http.StatusOK || got.Instance == nil {
			t.Fatalf("get project: %d %+v", code, got)
		}
		return got
	}
	setLimit := func(n int) int {
		t.Helper()
		var upd gen.ProjectUpdated
		code := e.Do("PATCH", "/api/v1/projects/"+pid+"/settings", gen.UpdateProjectRequest{Settings: &gen.ProjectSettingsPatch{ConnectionLimit: &n}}, &upd)
		if code == http.StatusOK && upd.Operation != nil {
			if op := e.WaitOperation(upd.Operation.Id); op.Status != gen.OperationStatusSucceeded {
				t.Fatalf("apply limit %d: %s\n%s", n, op.Status, testenv.FormatLog(op))
			}
		}
		return code
	}
	roleLimit := func() int {
		t.Helper()
		conn, err := e.Service.AdminConn(ctx, p.InstanceID, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var n int
		if err := conn.QueryRow(ctx, `SELECT rolconnlimit FROM pg_roles WHERE rolname = $1`, p.OwnerRole).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A medium instance (4 GB): 200 connections, up to 190 for the project.
	if got := show(); got != "200" {
		t.Fatalf("max_connections on a 4 GB instance: %s", got)
	}
	got := project()
	if got.Instance.MaxConnections == nil || *got.Instance.MaxConnections != 200 || got.Instance.ConnectionLimitMax == nil || *got.Instance.ConnectionLimitMax != 190 {
		t.Fatalf("instance as listed: %+v", got.Instance)
	}
	if got.Settings.ConnectionLimit != 90 {
		t.Fatalf("a new dedicated project's limit: %d", got.Settings.ConnectionLimit)
	}

	// Taskiem's 150, then the ceiling, then past it.
	if code := setLimit(150); code != http.StatusOK || roleLimit() != 150 {
		t.Fatalf("limit 150: %d, role limit %d", code, roleLimit())
	}
	if code := setLimit(190); code != http.StatusOK {
		t.Fatalf("limit 190: %d", code)
	}
	if code := setLimit(191); code != http.StatusBadRequest {
		t.Fatalf("limit 191 on a 200-connection instance: %d", code)
	}

	// Down to 1 GB: 100 connections, and the project's limit follows.
	cpus, mem := float32(1), 1024
	resize(t, e, c.Project.Id, gen.InstanceUpdate{Cpus: &cpus, MemoryMb: &mem})
	if got := show(); got != "100" {
		t.Fatalf("max_connections after the resize: %s", got)
	}
	got = project()
	if got.Settings.ConnectionLimit != 90 || *got.Instance.ConnectionLimitMax != 90 || roleLimit() != 90 {
		t.Fatalf("after the resize: limit %d, max %v, role %d", got.Settings.ConnectionLimit, got.Instance.ConnectionLimitMax, roleLimit())
	}
}
