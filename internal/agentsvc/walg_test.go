package agentsvc

import (
	"errors"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/docker"
)

// A failed backup-list is an error, never an empty archive; an archive
// with no backups yet is an empty list however WAL-G reports it.
func TestParseBackupList(t *testing.T) {
	failed := errors.New("wal-g failed (exit 1)")
	if list, err := parseBackupList(docker.ExecResult{ExitCode: 1, Stderr: "ERROR: failed to list s3 folder: RequestError: send request failed"}, failed); err == nil {
		t.Fatalf("a failed listing gave %v and no error", list)
	}
	for name, r := range map[string]docker.ExecResult{
		"no output":         {},
		"null":              {Stdout: "null\n"},
		"exit 0 and a note": {Stderr: "INFO: No backups found\n"},
	} {
		if list, err := parseBackupList(r, nil); err != nil || len(list) != 0 {
			t.Errorf("%s: %v, %v; want an empty list", name, list, err)
		}
	}
	if list, err := parseBackupList(docker.ExecResult{ExitCode: 1, Stderr: "ERROR: No backups found"}, failed); err != nil || len(list) != 0 {
		t.Errorf("an empty archive whose listing exits 1: %v, %v; want an empty list", list, err)
	}
	list, err := parseBackupList(docker.ExecResult{Stdout: `[{"backup_name":"base_2","start_time":"2026-10-02T02:00:00Z","finish_time":"2026-10-02T02:05:00Z"},
		{"backup_name":"base_1","start_time":"2026-10-01T02:00:00Z","finish_time":"2026-10-01T02:05:00Z"}]`}, nil)
	if err != nil || len(list) != 2 || list[0].Name != "base_1" {
		t.Fatalf("listing: %+v, %v; want both, oldest first", list, err)
	}
}

// The new backup is the newest listed only if it started with this push.
func TestNewBackup(t *testing.T) {
	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	old := agentapi.WALGBackup{Name: "base_1", StartTime: start.Add(-24 * time.Hour)}
	fresh := agentapi.WALGBackup{Name: "base_2", StartTime: start.Add(time.Second)}
	if b, err := newBackup([]agentapi.WALGBackup{old, fresh}, start); err != nil || b.Name != "base_2" {
		t.Fatalf("got %v, %v; want base_2", b, err)
	}
	if b, err := newBackup([]agentapi.WALGBackup{old}, start); err == nil {
		t.Fatalf("a listing without the new backup reported %s as new", b.Name)
	}
	if _, err := newBackup(nil, start); err == nil {
		t.Fatal("an empty listing after a push reported a backup")
	}
}
