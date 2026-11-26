package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

var (
	// ErrNotFound means no such organisation (or session).
	ErrNotFound = errors.New("not found")
	// ErrConflict means the organisation is in the wrong state.
	ErrConflict = errors.New("conflict")
	// ErrInvalid wraps a bad request.
	ErrInvalid = errors.New("invalid request")
)

// MaxBreakGlass is the longest a break-glass session may last (V2 §2.4).
const MaxBreakGlass = 4 * time.Hour

// OrgDeletionGrace is how long a deleted organisation can be restored.
const OrgDeletionGrace = 7 * 24 * time.Hour

// finalBackupRetention keeps a deleted organisation's final backups.
const finalBackupRetention = 30 * 24 * time.Hour

// setOrgLogins switches the logins of every live project in org on or off
// (each still respecting its own storage lock), and re-renders the pooler
// routes, which leave out suspended organisations.
func (s *Service) setOrgLogins(ctx context.Context, orgID uuid.UUID) error {
	ps, err := store.New(s.db).OrgLiveProjects(ctx, orgID)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		if p.Status != provision.StatusActive && p.Status != provision.StatusError {
			continue
		}
		allowed, err := s.LoginAllowed(ctx, p)
		if err == nil {
			err = s.projects.SetLogins(ctx, p, allowed)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", p.ID, err))
		}
		if !allowed {
			if pm := s.projects.Pooler(); pm != nil {
				_ = pm.Kill(ctx, store.PoolerNames(p)...)
			}
		}
	}
	if pm := s.projects.Pooler(); pm != nil {
		if err := pm.Sync(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Suspend suspends an organisation (V2 §10.8): its pooler routes go, its
// logins lose access, scheduled backups pause (one last backup is taken
// now), and its members see the reason. Data is kept.
func (s *Service) Suspend(ctx context.Context, orgID uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return fmt.Errorf("%w: a reason (up to 1000 characters) is required", ErrInvalid)
	}
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if o.Status != OrgActive {
		return fmt.Errorf("%w: the organisation is %s", ErrConflict, o.Status)
	}
	ps, err := q.OrgLiveProjects(ctx, orgID)
	if err != nil {
		return err
	}
	// The last backup goes first, while the project still answers.
	if s.FinalBackup != nil {
		for _, p := range ps {
			if p.Status == provision.StatusActive {
				if err := s.FinalBackup(ctx, p); err != nil {
					s.log.Warn("suspension backup", "project_id", p.ID, "err", err)
				}
			}
		}
	}
	if _, err := q.SuspendOrg(ctx, store.SuspendOrgParams{OrgID: orgID, Reason: &reason}); err != nil {
		return err
	}
	err = s.setOrgLogins(ctx, orgID)
	s.mailOwners(ctx, orgID, fmt.Sprintf("[Xenibase] %s is suspended", o.Name), fmt.Sprintf(
		"The platform admin suspended %s.\n\nReason: %s\n\nIts databases are offline and its data is kept. Members can still sign in to see this notice.\n\n%s",
		mail.Inert(o.Name), reason, s.link("/projects")))
	return err
}

// Reinstate reverses a suspension (or cancels a pending deletion).
func (s *Service) Reinstate(ctx context.Context, orgID uuid.UUID) error {
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	n, err := q.ReinstateOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: the organisation is %s", ErrConflict, o.Status)
	}
	err = s.setOrgLogins(ctx, orgID)
	what := "reinstated"
	if o.Status == OrgDeleting {
		what = "no longer being deleted"
	}
	s.mailOwners(ctx, orgID, fmt.Sprintf("[Xenibase] %s is %s", o.Name, what), fmt.Sprintf(
		"%s is %s: its databases accept connections again.\n\n%s", mail.Inert(o.Name), what, s.link("/projects")))
	return err
}

func (s *Service) mailOwners(ctx context.Context, orgID uuid.UUID, subject, body string) {
	addrs, err := store.New(s.db).OrgOwnerEmails(ctx, orgID)
	if err != nil {
		s.log.Warn("org owner emails", "err", err)
		return
	}
	s.notify(ctx, addrs, subject, body)
}

// StartBreakGlass opens a break-glass session (V2 §2.4): adminID acts as
// an admin of orgID until it expires or an owner ends it. Every owner is
// emailed now.
func (s *Service) StartBreakGlass(ctx context.Context, orgID, adminID uuid.UUID, adminEmail, reason string, d time.Duration) (store.BreakGlassSession, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return store.BreakGlassSession{}, fmt.Errorf("%w: a reason (up to 1000 characters) is required", ErrInvalid)
	}
	if d < time.Minute || d > MaxBreakGlass {
		return store.BreakGlassSession{}, fmt.Errorf("%w: the duration must be between 1 minute and 4 hours", ErrInvalid)
	}
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.BreakGlassSession{}, ErrNotFound
	}
	if err != nil {
		return store.BreakGlassSession{}, err
	}
	if _, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: adminID}); err == nil {
		return store.BreakGlassSession{}, fmt.Errorf("%w: you are a member of this organisation already", ErrConflict)
	}
	// Expiry is the database clock, as authz checks it.
	var expires time.Time
	if err := s.db.QueryRow(ctx, "SELECT now() + $1::interval", d.String()).Scan(&expires); err != nil {
		return store.BreakGlassSession{}, err
	}
	bg, err := q.InsertBreakGlass(ctx, store.InsertBreakGlassParams{OrgID: orgID, AdminID: adminID, Reason: reason, ExpiresAt: expires})
	if err != nil {
		return bg, err
	}
	s.mailOwners(ctx, orgID, fmt.Sprintf("[Xenibase] Break-glass access to %s", o.Name), fmt.Sprintf(
		"The platform admin (%s) opened break-glass access to %s until %s UTC.\n\nReason: %s\n\nThey can act as an org admin until then; everything they do is in the org's audit log, flagged as break-glass. Any owner can end the session early:\n%s",
		adminEmail, mail.Inert(o.Name), expires.UTC().Format("2006-01-02 15:04"), reason, s.link("/org/audit")))
	return bg, nil
}

// EndBreakGlass ends a session early.
func (s *Service) EndBreakGlass(ctx context.Context, orgID, id uuid.UUID, by *uuid.UUID) error {
	n, err := store.New(s.db).EndBreakGlass(ctx, store.EndBreakGlassParams{ID: id, OrgID: orgID, EndedBy: by})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EndExpiredBreakGlass records expired sessions as ended.
func (s *Service) EndExpiredBreakGlass(ctx context.Context) error {
	_, err := store.New(s.db).EndExpiredBreakGlass(ctx)
	return err
}

// RequestOrgDeletion starts deleting an organisation (V2 §10.10). Its
// projects go offline now and are deleted, each with a final backup, once
// the 7-day grace period ends; until then an owner can cancel. A personal
// organisation is not deleted this way.
func (s *Service) RequestOrgDeletion(ctx context.Context, orgID uuid.UUID, confirm string, deleteProjects bool, by uuid.UUID) (time.Time, error) {
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	if o.PersonalOwnerID != nil {
		return time.Time{}, fmt.Errorf("%w: a personal organisation is deleted with its account", ErrConflict)
	}
	if confirm != o.Name {
		return time.Time{}, fmt.Errorf("%w: type the organisation's name to confirm", ErrInvalid)
	}
	ps, err := q.OrgLiveProjects(ctx, orgID)
	if err != nil {
		return time.Time{}, err
	}
	if len(ps) > 0 && !deleteProjects {
		return time.Time{}, fmt.Errorf("%w: the organisation has %d project(s); delete them first, or confirm deleting them all", ErrConflict, len(ps))
	}
	after := s.Now().Add(OrgDeletionGrace)
	n, err := q.MarkOrgDeleting(ctx, store.MarkOrgDeletingParams{OrgID: orgID, DeleteAfter: &after, RequestedBy: &by})
	if err != nil {
		return time.Time{}, err
	}
	if n == 0 {
		return time.Time{}, fmt.Errorf("%w: the organisation is %s", ErrConflict, o.Status)
	}
	err = s.setOrgLogins(ctx, orgID)
	s.mailOwners(ctx, orgID, fmt.Sprintf("[Xenibase] %s will be deleted on %s", o.Name, after.UTC().Format("2006-01-02")), fmt.Sprintf(
		"%s and its %d project(s) will be deleted on %s UTC. Each project gets a final backup, kept for 30 days.\n\nUntil then any owner can cancel:\n%s",
		mail.Inert(o.Name), len(ps), after.UTC().Format("2006-01-02 15:04"), s.link("/org/settings")))
	return after, err
}

// FinishOrgDeletions deletes the organisations whose grace period ended:
// their projects (with final backups), then memberships and invitations.
// It takes several sweeps when projects have to be deleted first.
func (s *Service) FinishOrgDeletions(ctx context.Context) error {
	q := store.New(s.db)
	due, err := q.OrgsDueForDeletion(ctx, s.Now())
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range due {
		ps, err := q.OrgLiveProjects(ctx, o.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range ps {
			if p.Status == provision.StatusDeleting {
				continue
			}
			if _, err := s.projects.Delete(ctx, p.ID, p.Name, false, o.DeleteRequestedBy); err != nil && !errors.Is(err, provision.ErrConflict) {
				errs = append(errs, fmt.Errorf("delete project %s: %w", p.ID, err))
			}
		}
		if len(ps) > 0 {
			continue // finish on a later sweep, once the projects are gone
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			tq := store.New(tx)
			if err := tq.ExpireOrgFinalBackups(ctx, store.ExpireOrgFinalBackupsParams{OrgID: o.ID, ExpiresAt: ptr(s.Now().Add(finalBackupRetention))}); err != nil {
				return err
			}
			if err := tq.RevokeOrgInvitations(ctx, &o.ID); err != nil {
				return err
			}
			if err := tq.DeleteOrgMemberships(ctx, o.ID); err != nil {
				return err
			}
			return tq.MarkOrgDeleted(ctx, o.ID)
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Info("organisation deleted", "org_id", o.ID)
	}
	return errors.Join(errs...)
}

func ptr[T any](v T) *T { return &v }
