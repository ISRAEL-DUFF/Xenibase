package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestReplicaStandbySettings is Taskiem's replica ask: a read replica with
// hot_standby_feedback on and a chosen standby delay runs with them, its
// feedback reaches the primary, the primary and HA keep Postgres's
// defaults, and a change applies to the replica alone without a restart.
func TestReplicaStandbySettings(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	c, ns := haProject(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Dedicated.RunHAWatcher(ctx, time.Second)
	pid := c.Project.Id
	path := "/api/v1/projects/" + pid.String() + "/replicas"

	// Bad settings are refused before anything is queued.
	tooLong := 901
	if code := e.Do("POST", path, gen.ReplicaCreateRequest{MaxStandbyDelaySeconds: &tooLong}, nil); code != http.StatusBadRequest {
		t.Fatalf("a 901 s standby delay: %d", code)
	}

	on, delay := true, 120
	var op gen.Operation
	if code := e.Do("POST", path, gen.ReplicaCreateRequest{NodeId: &ns[2].Id, HotStandbyFeedback: &on, MaxStandbyDelaySeconds: &delay}, &op); code != http.StatusAccepted {
		t.Fatalf("create replica: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create replica: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	var rep gen.ReadReplica
	waitReplicas(t, e, pid, 60*time.Second, "the replica streaming with its settings confirmed", func(l gen.ReplicaList) bool {
		if len(l.Replicas) != 1 {
			return false
		}
		rep = l.Replicas[0]
		return rep.Status == gen.ReadReplicaStatusStreaming && rep.SettingsAppliedAt != nil && rep.SettingsError == nil
	})
	if !rep.HotStandbyFeedback || rep.MaxStandbyDelaySeconds != 120 {
		t.Fatalf("the replica as listed: %+v", rep)
	}

	show := func(member uuid.UUID) (string, string, string) {
		t.Helper()
		return memberSQL(t, member, "postgres", "SHOW hot_standby_feedback"),
			memberSQL(t, member, "postgres", "SHOW max_standby_streaming_delay"),
			memberSQL(t, member, "postgres", "SHOW max_standby_archive_delay")
	}
	if fb, sd, ad := show(rep.Id); fb != "on" || sd != "2min" || ad != "2min" {
		t.Fatalf("on the replica: hot_standby_feedback %s, streaming delay %s, archive delay %s", fb, sd, ad)
	}
	// The primary keeps Postgres's defaults.
	leader := e.MustConnect(c.Connection.SessionUrl)
	defer leader.Close(ctx)
	var pfb string
	if err := leader.QueryRow(ctx, `SHOW hot_standby_feedback`).Scan(&pfb); err != nil || pfb != "off" {
		t.Fatalf("hot_standby_feedback on the primary: %q %v", pfb, err)
	}

	// The feedback reaches the primary: it holds the replica's xmin, on the
	// replica's replication slot (Patroni's use_slots: named after the
	// member, dashes as underscores) or, without one, on its walsender.
	p, err := e.Service.Get(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := e.Service.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer adm.Close(ctx)
	xminHeld := func() bool {
		t.Helper()
		var held bool
		if err := adm.QueryRow(ctx, `SELECT
			COALESCE((SELECT bool_or(backend_xmin IS NOT NULL) FROM pg_stat_replication WHERE application_name = $1), false) OR
			COALESCE((SELECT bool_or(xmin IS NOT NULL) FROM pg_replication_slots WHERE slot_name = replace($1, '-', '_')), false)`,
			rep.Id.String()).Scan(&held); err != nil {
			t.Fatal(err)
		}
		return held
	}
	waitFor(t, 40*time.Second, "the replica's xmin on the primary", xminHeld)

	// A change applies to the running replica: no restart.
	var startedBefore string
	startedBefore = memberSQL(t, rep.Id, "postgres", "SELECT pg_postmaster_start_time()")
	off, short := false, 5
	var got gen.ReadReplica
	if code := e.Do("PATCH", path+"/"+rep.Id.String(), gen.ReplicaSettings{HotStandbyFeedback: &off, MaxStandbyDelaySeconds: &short}, &got); code != http.StatusOK {
		t.Fatalf("change the settings: %d", code)
	}
	if got.HotStandbyFeedback || got.MaxStandbyDelaySeconds != 5 || got.SettingsError != nil || got.SettingsAppliedAt == nil ||
		!got.SettingsAppliedAt.After(*rep.SettingsAppliedAt) {
		t.Fatalf("after the change: %+v", got)
	}
	if fb, sd, ad := show(rep.Id); fb != "off" || sd != "5s" || ad != "5s" {
		t.Fatalf("on the replica after the change: hot_standby_feedback %s, streaming delay %s, archive delay %s", fb, sd, ad)
	}
	if after := memberSQL(t, rep.Id, "postgres", "SELECT pg_postmaster_start_time()"); after != startedBefore {
		t.Fatalf("the replica restarted: %s, then %s", startedBefore, after)
	}
	waitFor(t, 40*time.Second, "the primary releasing the replica's xmin", func() bool { return !xminHeld() })

	// Only the field given changes.
	if code := e.Do("PATCH", path+"/"+rep.Id.String(), gen.ReplicaSettings{HotStandbyFeedback: &on}, &got); code != http.StatusOK ||
		!got.HotStandbyFeedback || got.MaxStandbyDelaySeconds != 5 {
		t.Fatalf("feedback only: %d %+v", code, got)
	}

	// Refused: out of range, someone else's id.
	if code := e.Do("PATCH", path+"/"+rep.Id.String(), gen.ReplicaSettings{MaxStandbyDelaySeconds: &tooLong}, nil); code != http.StatusBadRequest {
		t.Fatalf("a 901 s delay: %d", code)
	}
	if code := e.Do("PATCH", path+"/"+uuid.NewString(), gen.ReplicaSettings{HotStandbyFeedback: &off}, nil); code != http.StatusNotFound {
		t.Fatalf("an unknown replica: %d", code)
	}
}
