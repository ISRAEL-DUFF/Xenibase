// Package freetier pauses and archives inactive Free projects (V3 §4): a
// Free project with no client connections for 7 days is paused (its
// database refuses connections and its pooler route points at the waker);
// one paused for 90 days is archived (a verified final backup, then the
// database is dropped); one archived for 12 months is deleted. The first
// connection to a paused or archived project wakes it. Paid plans are
// never paused or archived.
package freetier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/waker"
)

// Lifecycles (projects.lifecycle).
const (
	Active   = "active"
	Paused   = "paused"
	Archived = "archived"
)

// Operation kinds (V3 §9).
const (
	KindPause     = "pause_project"
	KindResume    = "resume_project"
	KindArchive   = "archive_project"
	KindUnarchive = "unarchive_project"
)

// Errors for the API layer.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Config sets the thresholds (V3 §4.2–§4.3) and the clock.
type Config struct {
	// PauseAfter is how long without client connections before a pause;
	// the owners are told Warn before.
	PauseAfter time.Duration
	Warn       time.Duration
	// ArchiveAfter is how long paused before archiving; DeleteAfter how
	// long archived before deletion, with notices NoticeDays before.
	ArchiveAfter time.Duration
	DeleteAfter  time.Duration
	NoticeDays   []int
	PublicURL    string
	Now          func() time.Time
}

func (c *Config) setDefaults() {
	if c.PauseAfter <= 0 {
		c.PauseAfter = 7 * 24 * time.Hour
	}
	if c.Warn <= 0 {
		c.Warn = 24 * time.Hour
	}
	if c.ArchiveAfter <= 0 {
		c.ArchiveAfter = 90 * 24 * time.Hour
	}
	if c.DeleteAfter <= 0 {
		c.DeleteAfter = 365 * 24 * time.Hour
	}
	if len(c.NoticeDays) == 0 {
		c.NoticeDays = []int{30, 7}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Mailer sends notices.
type Mailer interface {
	Send(ctx context.Context, m mail.Message) error
}

// archiver takes and verifies archive backups (backup.Service).
type archiver interface {
	Archive(ctx context.Context, p store.Project, opID *uuid.UUID, log *jobs.StepLogger) (store.Backup, error)
	VerifyArchive(ctx context.Context, p store.Project, b store.Backup, log *jobs.StepLogger) (store.Backup, error)
}

// Service runs the Free tier lifecycle.
type Service struct {
	db       *pgxpool.Pool
	projects *provision.Service
	backups  *backup.Service
	// archives is backups, or a stand-in in tests without node agents.
	archives archiver
	mail     Mailer
	cfg      Config
	log      *slog.Logger
}

// New returns the service.
func New(db *pgxpool.Pool, projects *provision.Service, backups *backup.Service, m Mailer, cfg Config, log *slog.Logger) *Service {
	cfg.setDefaults()
	return &Service{db: db, projects: projects, backups: backups, archives: backups, mail: m, cfg: cfg, log: log}
}

// Kinds returns the operation kinds this service runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindPause:     {Handler: s.runPause, OnFail: s.failPause, MaxAttempts: 3, Timeout: 5 * time.Minute},
		KindResume:    {Handler: s.runResume, MaxAttempts: 5, Timeout: 5 * time.Minute},
		KindArchive:   {Handler: s.runArchive, OnFail: s.failArchive, MaxAttempts: 3, Timeout: 2 * time.Hour},
		KindUnarchive: {Handler: s.runUnarchive, OnFail: s.failUnarchive, MaxAttempts: 3, Timeout: 2 * time.Hour},
	}
}

func (s *Service) now() time.Time { return s.cfg.Now() }

func (s *Service) link(path string) string { return strings.TrimRight(s.cfg.PublicURL, "/") + path }

// notify emails the org's owners and the project's admins.
func (s *Service) notify(ctx context.Context, p store.Project, subject string, c mail.Content) {
	if s.mail == nil {
		return
	}
	to, err := store.New(s.db).ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: p.OrgID, ProjectID: p.ID})
	if err != nil || len(to) == 0 {
		return
	}
	c.Note = "You're getting this because you look after " + p.Name + " on Xenibase's Free plan."
	m := mail.Compose(to, subject, c)
	m.Headers = map[string]string{"X-Xenibase-Event": "free_tier"}
	if err := s.mail.Send(ctx, m); err != nil {
		s.log.Warn("free tier notice", "project_id", p.ID, "err", err)
	}
}

func (s *Service) projectLink(p store.Project) string {
	return s.link("/projects/" + p.ID.String())
}

// ---- Waking -----------------------------------------------------------------

// Wake is the waker's hook: the first connection to a paused project
// queues its resume, and to an archived one its restore.
func (s *Service) Wake(ctx context.Context, database string) (waker.State, error) {
	p, err := store.New(s.db).ProjectByDatabase(ctx, database)
	if errors.Is(err, pgx.ErrNoRows) {
		return waker.Unknown, nil
	}
	if err != nil {
		return waker.Unknown, err
	}
	st := waker.Resuming
	switch p.Lifecycle {
	case Active:
		return waker.Awake, nil
	case Archived:
		st = waker.Restoring
	}
	if _, err := s.Resume(ctx, p.ID, nil); err != nil && !errors.Is(err, ErrConflict) {
		return st, err
	}
	return st, nil
}

// Resume queues the resume of a paused project, or the restore of an
// archived one (V3 §4.2–§4.3). A project already waking is left alone.
func (s *Service) Resume(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	return s.enqueue(ctx, projectID, by, func(p store.Project) (string, error) {
		switch p.Lifecycle {
		case Paused:
			return KindResume, nil
		case Archived:
			return KindUnarchive, nil
		}
		return "", fmt.Errorf("%w: the project isn't paused or archived", ErrConflict)
	})
}

// enqueue locks a live, active project with no operation pending and
// queues the operation pick chooses (one operation per project at a time).
func (s *Service) enqueue(ctx context.Context, projectID uuid.UUID, by *uuid.UUID, pick func(store.Project) (string, error)) (store.Operation, error) {
	var out store.Operation
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.GetLiveProjectForUpdate(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Status != provision.StatusActive {
			return fmt.Errorf("%w: the project is %s", ErrConflict, p.Status)
		}
		kind, err := pick(p)
		if err != nil {
			return err
		}
		if busy, err := q.ProjectHasActiveOperation(ctx, &p.ID); err != nil {
			return err
		} else if busy {
			return fmt.Errorf("%w: another operation is in progress on this project", ErrConflict)
		}
		out, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: kind, ProjectID: &p.ID, CreatedBy: by})
		return err
	})
	return out, err
}
