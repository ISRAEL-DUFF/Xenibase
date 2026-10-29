package agentsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/israel-duff/xenibase/internal/agentapi"
)

// pgEnv passes a connection to pg_dump/pg_restore through libpq environment
// variables, so passwords never appear in argv (and so in ps).
func pgEnv(c agentapi.PGConn) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PGHOST=" + c.Host,
		"PGPORT=" + strconv.Itoa(c.Port),
		"PGUSER=" + c.User,
		"PGPASSWORD=" + c.Password,
		"PGDATABASE=" + c.Database,
		"PGAPPNAME=xenibase-agent",
		"PGCONNECT_TIMEOUT=15",
	}
	if c.HostAddr != "" {
		env = append(env, "PGHOSTADDR="+c.HostAddr)
	}
	mode := c.SSLMode
	if mode == "" {
		mode = "prefer"
	}
	return append(env, "PGSSLMODE="+mode)
}

func (s *Service) bin(name string) string {
	if s.cfg.PGBinDir != "" {
		return filepath.Join(s.cfg.PGBinDir, name)
	}
	return name
}

func dumpArgs(o agentapi.DumpOptions) []string {
	args := []string{"--format=custom", "--no-password", "--quote-all-identifiers"}
	for _, n := range o.Schemas {
		args = append(args, "--schema="+n)
	}
	for _, n := range o.ExcludeSchemas {
		args = append(args, "--exclude-schema="+n)
	}
	for _, n := range o.ExcludeExtensions {
		args = append(args, "--exclude-extension="+n)
	}
	if o.NoOwner {
		args = append(args, "--no-owner")
	}
	if o.NoACL {
		args = append(args, "--no-acl")
	}
	if o.SchemaOnly {
		args = append(args, "--schema-only")
	}
	return args
}

func restoreArgs(o agentapi.RestoreOptions, db string) []string {
	args := []string{"--no-password", "--dbname=" + db}
	if o.KeepOwners {
		// Grants are restored, except those on system objects: a dump of a
		// shared cluster's database carries its activity hardening (V2
		// §10.2), which the restore login may not change and the target
		// applies itself.
		args = append(args, "--exclude-schema=pg_catalog")
	} else {
		args = append(args, "--no-owner", "--no-acl")
	}
	if o.Role != "" {
		args = append(args, "--role="+o.Role)
	}
	if o.SchemaOnly {
		args = append(args, "--schema-only")
	}
	if !o.AllowErrors {
		args = append(args, "--exit-on-error", "--single-transaction")
	}
	return args
}

// stderrBuffer keeps the last 64 KiB of a process's stderr.
type stderrBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if b.buf.Len() > 64<<10 {
		tail := b.buf.Bytes()[b.buf.Len()-32<<10:]
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return len(p), nil
}

func (b *stderrBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (b *stderrBuffer) summary() string {
	ls := b.lines()
	if len(ls) > 8 {
		ls = ls[len(ls)-8:]
	}
	return strings.Join(ls, "; ")
}

// procError describes a failed child process with the end of its stderr.
func procError(name string, err error, stderr *stderrBuffer) error {
	if msg := stderr.summary(); msg != "" {
		return fmt.Errorf("%s: %s", name, msg)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// startDump runs pg_dump with its stdout returned as a reader.
func (s *Service) startDump(ctx context.Context, c agentapi.PGConn, o agentapi.DumpOptions) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, s.bin("pg_dump"), dumpArgs(o)...)
	dieWithAgent(cmd)
	cmd.Env = pgEnv(c)
	stderr := &stderrBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start pg_dump: %w", err)
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return procError("pg_dump", err, stderr)
		}
		return nil
	}
	return out, wait, nil
}

// runRestore feeds in to pg_restore. With AllowErrors, statement failures
// come back as warnings; otherwise any failure aborts the (single
// transaction) restore.
func (s *Service) runRestore(ctx context.Context, in io.Reader, c agentapi.PGConn, o agentapi.RestoreOptions) ([]string, error) {
	cmd := exec.CommandContext(ctx, s.bin("pg_restore"), restoreArgs(o, c.Database)...)
	dieWithAgent(cmd)
	cmd.Env = pgEnv(c)
	cmd.Stdin = in
	stderr := &stderrBuffer{}
	cmd.Stderr = stderr
	err := cmd.Run()
	var warnings []string
	inCommand := false
	for _, l := range stderr.lines() {
		switch {
		case strings.HasPrefix(l, "Command was:"):
			// It names the object that failed; it may span lines.
			warnings = append(warnings, l)
			inCommand = true
		case inCommand && !strings.HasPrefix(l, "pg_restore:"):
			warnings[len(warnings)-1] += " " + strings.TrimSpace(l)
		case strings.Contains(l, "error") || strings.Contains(l, "ERROR") || strings.Contains(l, "warning"):
			warnings = append(warnings, l)
			inCommand = false
		default:
			inCommand = false
		}
	}
	for i, w := range warnings {
		if len(w) > 500 {
			warnings[i] = w[:500] + "…"
		}
	}
	if err != nil {
		var exit *exec.ExitError
		// pg_restore exits 1 when it ignored errors; with AllowErrors that
		// is the expected outcome and the errors are the warnings.
		if o.AllowErrors && errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
			return warnings, nil
		}
		return warnings, procError("pg_restore", err, stderr)
	}
	return warnings, nil
}

func (s *Service) toolVersion(name string) string {
	out, err := exec.Command(s.bin(name), "--version").Output()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
