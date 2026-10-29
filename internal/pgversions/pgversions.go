// Package pgversions runs the Postgres version lifecycle (V4.1 §6.1): an
// admin promotes a preview, deprecates a major with a retirement date at
// least 180 days ahead, or retires it; the owners and admins of every
// organisation with a project on a deprecated major are told when it is
// deprecated and again 90, 30 and 7 days before it retires.
package pgversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// MinNotice is the shortest deprecation: a retirement date at least this
// far ahead.
const MinNotice = 180 * 24 * time.Hour

// Reminders are the days before retirement a notice goes out again.
var Reminders = []int{90, 30, 7}

// ErrInvalid is a change the lifecycle refuses.
var ErrInvalid = errors.New("invalid version change")

// Config tunes the service.
type Config struct {
	// PublicURL is the dashboard's address, for the links in the notices.
	PublicURL string
	// Now is the clock (time.Now by default); tests move it.
	Now func() time.Time
}

// Service is the version lifecycle.
type Service struct {
	db       *pgxpool.Pool
	projects *provision.Service
	mail     *mail.Service
	cfg      Config
	log      *slog.Logger
	// send replaces mail.Send (tests).
	send func(ctx context.Context, m mail.Message) error
}

// New returns a Service. mail may be nil (no notices).
func New(db *pgxpool.Pool, projects *provision.Service, m *mail.Service, cfg Config, log *slog.Logger) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{db: db, projects: projects, mail: m, cfg: cfg, log: log}
}

// SetNow replaces the clock (tests).
func (s *Service) SetNow(now func() time.Time) { s.cfg.Now = now }

// Update is an admin's change to a major.
type Update struct {
	Status    string
	RetiresAt *time.Time
	Notes     *string
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Change applies u to major and, on a deprecation, notifies the owners.
func (s *Service) Change(ctx context.Context, major int, u Update) (provision.PGVersion, error) {
	if _, err := s.projects.Versions(ctx); err != nil { // the configured majors are in the table
		return provision.PGVersion{}, err
	}
	q := store.New(s.db)
	cur, err := q.GetPgVersion(ctx, int32(major))
	if errors.Is(err, pgx.ErrNoRows) {
		return provision.PGVersion{}, invalid("Xenibase doesn't know Postgres %d", major)
	}
	if err != nil {
		return provision.PGVersion{}, err
	}
	now := s.cfg.Now()
	notes := cur.Notes
	if u.Notes != nil {
		notes = strings.TrimSpace(*u.Notes)
	}
	params := store.UpdatePgVersionParams{Major: cur.Major, Status: u.Status, Notes: notes}
	deprecating := false
	switch u.Status {
	case provision.VersionPreview:
		if cur.Status != provision.VersionPreview {
			return provision.PGVersion{}, invalid("only a new major is a preview; Postgres %d is %s", major, cur.Status)
		}
	case provision.VersionSupported:
		// Promoting a preview, or taking a deprecation back.
	case provision.VersionDeprecated:
		if u.RetiresAt == nil {
			return provision.PGVersion{}, invalid("a deprecation needs its retirement date (retires_at)")
		}
		if u.RetiresAt.Before(now.Add(MinNotice)) {
			return provision.PGVersion{}, invalid("the retirement date must be at least 180 days ahead (on or after %s), so owners have time to upgrade",
				now.Add(MinNotice).Format("2 January 2006"))
		}
		at := now
		if cur.Status == provision.VersionDeprecated && cur.DeprecatedAt != nil {
			at = *cur.DeprecatedAt
		}
		params.DeprecatedAt, params.RetiresAt = &at, u.RetiresAt
		deprecating = cur.Status != provision.VersionDeprecated
	case provision.VersionRetired:
		effective := provision.EffectiveStatus(cur.Status, cur.RetiresAt, now)
		if effective != provision.VersionRetired {
			rows, err := q.ProjectsOnPgVersion(ctx, cur.Major)
			if err != nil {
				return provision.PGVersion{}, err
			}
			if len(rows) > 0 {
				return provision.PGVersion{}, invalid("%d projects are on Postgres %d: deprecate it with a retirement date at least 180 days ahead instead", len(rows), major)
			}
		}
		params.DeprecatedAt, params.RetiresAt = cur.DeprecatedAt, cur.RetiresAt
		if params.RetiresAt == nil {
			params.RetiresAt = &now
		}
	default:
		return provision.PGVersion{}, invalid("status is preview, supported, deprecated or retired")
	}
	row, err := q.UpdatePgVersion(ctx, params)
	if err != nil {
		return provision.PGVersion{}, err
	}
	if deprecating {
		s.notify(ctx, row, 0)
	}
	return s.one(ctx, major)
}

func (s *Service) one(ctx context.Context, major int) (provision.PGVersion, error) {
	vs, err := s.projects.Versions(ctx)
	if err != nil {
		return provision.PGVersion{}, err
	}
	for _, v := range vs {
		if v.Major == major {
			return v, nil
		}
	}
	return provision.PGVersion{}, invalid("Xenibase doesn't know Postgres %d", major)
}

// Sweep sends the reminders due: 90, 30 and 7 days before each deprecated
// major retires, to every organisation with a project still on it.
func (s *Service) Sweep(ctx context.Context) error {
	rows, err := store.New(s.db).ListPgVersions(ctx)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	for _, r := range rows {
		if r.Status != provision.VersionDeprecated || r.RetiresAt == nil || !now.Before(*r.RetiresAt) {
			continue
		}
		left := r.RetiresAt.Sub(now)
		// The nearest step reached: one reminder however long the server
		// was down. Before the first, the deprecation notice again, for
		// the organisations whose mail failed (notify skips the rest).
		step := 0
		for i := len(Reminders) - 1; i >= 0; i-- {
			if left <= time.Duration(Reminders[i])*24*time.Hour {
				step = Reminders[i]
				break
			}
		}
		s.notify(ctx, store.PgVersion{Major: r.Major, Status: r.Status, DeprecatedAt: r.DeprecatedAt, RetiresAt: r.RetiresAt, Notes: r.Notes}, step)
	}
	return nil
}

// Run sweeps every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("postgres version notices", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// notify emails each organisation with a project on v once for step
// (0: the deprecation itself; else days before retirement).
func (s *Service) notify(ctx context.Context, v store.PgVersion, step int) {
	q := store.New(s.db)
	projects, err := q.ProjectsOnPgVersion(ctx, v.Major)
	if err != nil {
		s.log.Warn("postgres version notice", "major", v.Major, "err", err)
		return
	}
	byOrg := map[uuid.UUID][]store.ProjectsOnPgVersionRow{}
	for _, p := range projects {
		byOrg[p.OrgID] = append(byOrg[p.OrgID], p)
	}
	orgs := make([]uuid.UUID, 0, len(byOrg))
	for id := range byOrg {
		orgs = append(orgs, id)
	}
	sort.Slice(orgs, func(i, j int) bool { return byOrg[orgs[i]][0].OrgName < byOrg[orgs[j]][0].OrgName })
	for _, org := range orgs {
		if err := s.notifyOrg(ctx, v, step, org, byOrg[org]); err != nil {
			s.log.Warn("postgres version notice", "major", v.Major, "org", org, "err", err)
		}
	}
}

// notifyOrg sends one organisation its notice, once. The pg_version_notices
// row is inserted in a transaction that stays open until the mail is sent:
// another server's insert waits on it and then finds the row, so only one
// sends, and a send that fails rolls the row back for the next sweep.
func (s *Service) notifyOrg(ctx context.Context, v store.PgVersion, step int, org uuid.UUID, projects []store.ProjectsOnPgVersionRow) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.InsertPgVersionNotice(ctx, store.InsertPgVersionNoticeParams{Major: v.Major, OrgID: org, DaysBefore: int32(step)})
		if err != nil || n == 0 {
			return err // sent already, or being sent
		}
		send := s.send
		if send == nil {
			if s.mail == nil {
				return nil
			}
			send = s.mail.Send
		}
		to, err := q.ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: org, ProjectID: uuid.Nil})
		if err != nil {
			return err
		}
		if len(to) == 0 {
			return nil
		}
		if err := send(ctx, s.message(v, step, projects, to)); err != nil {
			return fmt.Errorf("not sent; the next sweep tries again: %w", err)
		}
		return nil
	})
}

// message is the notice for one organisation's projects on v.
func (s *Service) message(v store.PgVersion, step int, projects []store.ProjectsOnPgVersionRow, to []string) mail.Message {
	var list strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&list, "- %s: %s/projects/%s/settings (Upgrade)\n", p.Name, strings.TrimRight(s.cfg.PublicURL, "/"), p.ID)
	}
	date := v.RetiresAt.Format("2 January 2006")
	subject := fmt.Sprintf("[Xenibase] Postgres %d is deprecated: upgrade before %s", v.Major, date)
	if step > 0 {
		subject = fmt.Sprintf("[Xenibase] Postgres %d retires in %d days (%s)", v.Major, step, date)
	}
	body := fmt.Sprintf("Postgres %d is deprecated on Xenibase and retires on %s.\n\n"+
		"These projects of %s run it:\n%s\n"+
		"Upgrade each from its settings (Upgrade Postgres): a preflight checks the schema on the new major first, and the move pauses writes for seconds.\n\n"+
		"After %s the projects keep running but are unsupported: no new projects on Postgres %d, and fixes and help are limited (see the terms).\n",
		v.Major, date, projects[0].OrgName, list.String(), date, v.Major)
	if v.Notes != "" {
		body += "\n" + v.Notes + "\n"
	}
	return mail.Message{To: to, Subject: subject, Body: body}
}
