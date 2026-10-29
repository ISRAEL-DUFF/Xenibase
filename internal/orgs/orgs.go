// Package orgs runs organisations, memberships, invitations, and project
// transfers (V2 §2–3).
package orgs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Errors returned to the API layer. Messages are safe to show.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid request")
	ErrForbidden    = errors.New("forbidden")
	ErrLastOwner    = errors.New("an organisation must keep at least one owner")
	ErrConflict     = errors.New("conflict")
	ErrSignInFirst  = errors.New("this invitation is for an existing account: sign in to accept it")
	ErrWrongAccount = errors.New("this invitation was sent to a different email address")
)

// InvitationTTL is how long an invitation stays valid (V2 §3.2).
const InvitationTTL = 7 * 24 * time.Hour

// Service runs organisations.
type Service struct {
	db        *pgxpool.Pool
	auth      *auth.Service
	projects  *provision.Service
	mailer    auth.Mailer
	publicURL string
	log       *slog.Logger
	now       func() time.Time
}

// New returns a Service. projects may be nil in tests that never touch
// database credentials.
func New(db *pgxpool.Pool, a *auth.Service, projects *provision.Service, mailer auth.Mailer, publicURL string, log *slog.Logger) *Service {
	return &Service{db: db, auth: a, projects: projects, mailer: mailer, publicURL: strings.TrimRight(publicURL, "/"), log: log, now: time.Now}
}

// Hooks are the account hooks auth runs.
func (s *Service) Hooks() auth.Hooks {
	return auth.Hooks{
		UserCreated:    s.createPersonalOrg,
		SetupCompleted: s.adoptOrphans,
		UserDisabled:   s.revokeEverywhere,
	}
}

// ---- Organisations -----------------------------------------------------------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugBase turns a name into a URL-friendly slug stem.
func slugBase(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 30 {
		s = strings.TrimRight(s[:30], "-")
	}
	if s == "" {
		s = "org"
	}
	return s
}

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return "", fmt.Errorf("%w: the name must be 1 to 64 characters", ErrInvalid)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: the name must not contain control characters", ErrInvalid)
		}
	}
	return name, nil
}

func (s *Service) insertOrg(ctx context.Context, q *store.Queries, name string, personal *uuid.UUID, plan string) (store.Organization, error) {
	p, err := q.GetPlanByName(ctx, plan)
	if err != nil {
		return store.Organization{}, fmt.Errorf("plan %s: %w", plan, err)
	}
	settings, _ := json.Marshal(store.DefaultOrgSettings())
	base := slugBase(name)
	for attempt := 0; attempt < 6; attempt++ {
		slug := base + "-" + randomSuffix()
		taken, err := q.OrgSlugTaken(ctx, store.OrgSlugTakenParams{Slug: slug, OrgID: uuid.Nil})
		if err != nil {
			return store.Organization{}, err
		}
		if taken {
			continue
		}
		return q.InsertOrg(ctx, store.InsertOrgParams{Name: name, Slug: slug, PersonalOwnerID: personal, PlanID: p.ID, Settings: settings})
	}
	return store.Organization{}, errors.New("could not pick a free organisation slug")
}

// PersonalOrgName is the default name of a user's personal organisation.
func PersonalOrgName(u store.User) string {
	who := strings.Split(u.Email, "@")[0]
	if u.Name != nil && strings.TrimSpace(*u.Name) != "" {
		who = strings.TrimSpace(*u.Name)
	}
	name := who + "'s projects"
	if utf8.RuneCountInString(name) > 64 {
		name = string([]rune(name)[:64])
	}
	return name
}

// createPersonalOrg gives a new user their personal organisation
// (V2 §2.1): Unlimited for the platform admin, Personal for everyone else.
func (s *Service) createPersonalOrg(ctx context.Context, tx pgx.Tx, u store.User) error {
	q := store.New(tx)
	plan := "Personal"
	if u.PlatformRole == auth.RolePlatformAdmin {
		plan = "Unlimited"
	}
	o, err := s.insertOrg(ctx, q, PersonalOrgName(u), &u.ID, plan)
	if err != nil {
		return err
	}
	return q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: o.ID, UserID: u.ID, Role: authz.OrgOwner})
}

// adoptOrphans makes the first platform admin owner of organisations that
// have no members (projects that predate every user).
func (s *Service) adoptOrphans(ctx context.Context, tx pgx.Tx, u store.User) error {
	q := store.New(tx)
	ids, err := q.OrphanOrgs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: id, UserID: u.ID, Role: authz.OrgOwner}); err != nil {
			return err
		}
	}
	return nil
}

// Create makes a new organisation owned by userID.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, name string) (store.Organization, error) {
	name, err := validateName(name)
	if err != nil {
		return store.Organization{}, err
	}
	var o store.Organization
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if o, err = s.insertOrg(ctx, q, name, nil, "Personal"); err != nil {
			return err
		}
		return q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: o.ID, UserID: userID, Role: authz.OrgOwner})
	})
	return o, err
}

// Patch changes an organisation's name, slug, or settings.
type Patch struct {
	Name                     *string
	Slug                     *string
	MembersCanCreateProjects *bool
	SensitiveByDefault       *bool
}

// Update applies p to orgID.
func (s *Service) Update(ctx context.Context, orgID uuid.UUID, p Patch) (store.Organization, error) {
	q := store.New(s.db)
	o, err := q.GetOrg(ctx, orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	name, slug := o.Name, o.Slug
	if p.Name != nil {
		if name, err = validateName(*p.Name); err != nil {
			return o, err
		}
	}
	if p.Slug != nil {
		slug = strings.ToLower(strings.TrimSpace(*p.Slug))
		if !slugRe.MatchString(slug) {
			return o, fmt.Errorf("%w: the slug must be 1 to 40 lowercase letters, digits, and dashes", ErrInvalid)
		}
		taken, err := q.OrgSlugTaken(ctx, store.OrgSlugTakenParams{Slug: slug, OrgID: orgID})
		if err != nil {
			return o, err
		}
		if taken {
			return o, fmt.Errorf("%w: the slug %q is taken", ErrConflict, slug)
		}
	}
	set, err := store.DecodeOrgSettings(o.Settings)
	if err != nil {
		return o, err
	}
	if p.MembersCanCreateProjects != nil {
		set.MembersCanCreateProjects = *p.MembersCanCreateProjects
	}
	if p.SensitiveByDefault != nil {
		set.SensitiveByDefault = *p.SensitiveByDefault
	}
	raw, _ := json.Marshal(set)
	return q.UpdateOrg(ctx, store.UpdateOrgParams{OrgID: orgID, Name: name, Slug: slug, Settings: raw})
}

// ---- Members ------------------------------------------------------------------

// withMembersLocked runs f in a transaction that holds orgID's membership
// lock, so concurrent changes cannot remove the last owner.
func (s *Service) withMembersLocked(ctx context.Context, orgID uuid.UUID, f func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockOrgMembers(ctx, orgID.String()); err != nil {
			return err
		}
		return f(q)
	})
}

func validOrgRole(r string) bool {
	return r == authz.OrgOwner || r == authz.OrgAdmin || r == authz.OrgMember || r == authz.OrgBilling
}

// SetMemberRole changes target's role in orgID, on behalf of an actor with
// actorRole. Only owners hand out or take away the owner role.
func (s *Service) SetMemberRole(ctx context.Context, orgID uuid.UUID, actorRole string, target uuid.UUID, role string) error {
	if !validOrgRole(role) {
		return fmt.Errorf("%w: role must be owner, admin, member, or billing", ErrInvalid)
	}
	err := s.withMembersLocked(ctx, orgID, func(q *store.Queries) error {
		cur, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: target})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if (cur.Role == authz.OrgOwner || role == authz.OrgOwner) && actorRole != authz.OrgOwner {
			return fmt.Errorf("%w: only owners can add or remove owners", ErrForbidden)
		}
		if (cur.Role == authz.OrgBilling || role == authz.OrgBilling) && actorRole != authz.OrgOwner {
			// Admins can't see billing, so they don't hand out access to it.
			return fmt.Errorf("%w: only owners can give or take the billing role", ErrForbidden)
		}
		if cur.Role == authz.OrgOwner && role != authz.OrgOwner {
			if n, err := q.CountOrgOwners(ctx, orgID); err != nil {
				return err
			} else if n <= 1 {
				return ErrLastOwner
			}
		}
		if role == authz.OrgBilling {
			// The billing role has no project access (V3 §3.2).
			if _, err := q.DeleteOrgProjectMemberships(ctx, store.DeleteOrgProjectMembershipsParams{OrgID: orgID, UserID: target}); err != nil {
				return err
			}
		}
		_, err = q.SetOrgMemberRole(ctx, store.SetOrgMemberRoleParams{OrgID: orgID, UserID: target, Role: role})
		return err
	})
	if err != nil {
		return err
	}
	return s.refreshAccess(ctx, orgID, target)
}

// RemoveMember removes target from orgID (V2 §3.4): memberships go at
// once, then their database logins in the organisation's projects.
func (s *Service) RemoveMember(ctx context.Context, orgID uuid.UUID, actorRole string, target uuid.UUID) error {
	err := s.withMembersLocked(ctx, orgID, func(q *store.Queries) error {
		cur, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: target})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Role == authz.OrgOwner {
			if actorRole != authz.OrgOwner {
				return fmt.Errorf("%w: only owners can remove owners", ErrForbidden)
			}
			if n, err := q.CountOrgOwners(ctx, orgID); err != nil {
				return err
			} else if n <= 1 {
				return ErrLastOwner
			}
		}
		return s.deleteMembership(ctx, q, orgID, target)
	})
	if err != nil {
		return err
	}
	return s.revokeLogins(ctx, orgID, target)
}

// Leave removes userID from orgID at their own request. The last owner
// cannot leave, nor can anyone leave their personal organisation.
func (s *Service) Leave(ctx context.Context, orgID, userID uuid.UUID) error {
	err := s.withMembersLocked(ctx, orgID, func(q *store.Queries) error {
		o, err := q.GetOrg(ctx, orgID)
		if err != nil {
			return err
		}
		if o.PersonalOwnerID != nil && *o.PersonalOwnerID == userID {
			return fmt.Errorf("%w: you can't leave your personal organisation", ErrInvalid)
		}
		cur, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Role == authz.OrgOwner {
			if n, err := q.CountOrgOwners(ctx, orgID); err != nil {
				return err
			} else if n <= 1 {
				return fmt.Errorf("%w: transfer ownership before leaving", ErrLastOwner)
			}
		}
		return s.deleteMembership(ctx, q, orgID, userID)
	})
	if err != nil {
		return err
	}
	return s.revokeLogins(ctx, orgID, userID)
}

// TransferOwnership makes to an owner and from (an owner) an admin.
func (s *Service) TransferOwnership(ctx context.Context, orgID, from, to uuid.UUID) error {
	if from == to {
		return fmt.Errorf("%w: choose another member", ErrInvalid)
	}
	err := s.withMembersLocked(ctx, orgID, func(q *store.Queries) error {
		if _, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: to}); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the new owner must already be a member", ErrInvalid)
		} else if err != nil {
			return err
		}
		if _, err := q.SetOrgMemberRole(ctx, store.SetOrgMemberRoleParams{OrgID: orgID, UserID: to, Role: authz.OrgOwner}); err != nil {
			return err
		}
		_, err := q.SetOrgMemberRole(ctx, store.SetOrgMemberRoleParams{OrgID: orgID, UserID: from, Role: authz.OrgAdmin})
		return err
	})
	if err != nil {
		return err
	}
	if err := s.refreshAccess(ctx, orgID, to); err != nil {
		return err
	}
	return s.refreshAccess(ctx, orgID, from)
}

func (s *Service) deleteMembership(ctx context.Context, q *store.Queries, orgID, userID uuid.UUID) error {
	if _, err := q.DeleteOrgProjectMemberships(ctx, store.DeleteOrgProjectMembershipsParams{OrgID: orgID, UserID: userID}); err != nil {
		return err
	}
	// Their API tokens for the organisation go too (V2 §3.4).
	if _, err := q.RevokeUserOrgTokens(ctx, store.RevokeUserOrgTokensParams{OrgID: orgID, UserID: userID}); err != nil {
		return err
	}
	_, err := q.DeleteOrgMember(ctx, store.DeleteOrgMemberParams{OrgID: orgID, UserID: userID})
	return err
}

// revokeLogins drops every database login userID has in orgID's projects.
func (s *Service) revokeLogins(ctx context.Context, orgID, userID uuid.UUID) error {
	if s.projects == nil {
		return nil
	}
	q := store.New(s.db)
	logins, err := q.ListUserDBUsersInOrg(ctx, store.ListUserDBUsersInOrgParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range logins {
		p, err := q.GetOrgProject(ctx, store.GetOrgProjectParams{ID: l.ProjectID, OrgID: orgID})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.projects.RevokeCredentials(ctx, p, userID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// revokeEverywhere drops a disabled user's logins in every organisation,
// and their API tokens.
func (s *Service) revokeEverywhere(ctx context.Context, userID uuid.UUID) error {
	if _, err := store.New(s.db).RevokeUserTokens(ctx, userID); err != nil {
		return err
	}
	orgs, err := store.New(s.db).ListUserOrgs(ctx, userID)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range orgs {
		if err := s.revokeLogins(ctx, o.ID, userID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EffectiveProjectRole is userID's role on a project: admin for org owners
// and admins, else their project membership ("" for none).
func EffectiveProjectRole(ctx context.Context, q *store.Queries, orgID, projectID, userID uuid.UUID) (string, error) {
	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: orgID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if m.Role == authz.OrgOwner || m.Role == authz.OrgAdmin {
		return authz.ProjectAdmin, nil
	}
	if m.Role == authz.OrgBilling {
		return "", nil
	}
	pm, err := q.GetProjectMember(ctx, store.GetProjectMemberParams{ProjectID: projectID, UserID: userID, OrgID: orgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return pm.Role, err
}

// refreshAccess brings userID's database logins in orgID in line with
// their roles after a change: read-only or read/write, or revoked when they
// no longer have the project.
func (s *Service) refreshAccess(ctx context.Context, orgID, userID uuid.UUID) error {
	if s.projects == nil {
		return nil
	}
	q := store.New(s.db)
	logins, err := q.ListUserDBUsersInOrg(ctx, store.ListUserDBUsersInOrgParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range logins {
		p, err := q.GetOrgProject(ctx, store.GetOrgProjectParams{ID: l.ProjectID, OrgID: orgID})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		role, err := EffectiveProjectRole(ctx, q, orgID, p.ID, userID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if role == "" {
			err = s.projects.RevokeCredentials(ctx, p, userID)
		} else {
			err = s.projects.SetCredentialAccess(ctx, p, userID, authz.CredentialAccess(role))
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- Project members ---------------------------------------------------------

func validProjectRole(r string) bool {
	return r == authz.ProjectAdmin || r == authz.ProjectDeveloper || r == authz.ProjectReadOnly
}

// AddProjectMember gives an existing org member a role on p.
func (s *Service) AddProjectMember(ctx context.Context, p store.Project, userID uuid.UUID, role string, by uuid.UUID) error {
	if !validProjectRole(role) {
		return fmt.Errorf("%w: role must be admin, developer, or read_only", ErrInvalid)
	}
	q := store.New(s.db)
	if m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: p.OrgID, UserID: userID}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	} else if m.Role == authz.OrgBilling {
		return fmt.Errorf("%w: billing members have no project access", ErrInvalid)
	}
	if err := q.UpsertProjectMember(ctx, store.UpsertProjectMemberParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID, Role: role, AddedBy: &by}); err != nil {
		return err
	}
	return s.refreshAccess(ctx, p.OrgID, userID)
}

// RemoveProjectMember takes p away from userID: the membership, and their
// memberships of p's branches, which hold its data (V2 §8.1), then their
// logins on them.
func (s *Service) RemoveProjectMember(ctx context.Context, p store.Project, userID uuid.UUID) error {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.DeleteProjectMember(ctx, store.DeleteProjectMemberParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err = q.DeleteBranchMemberships(ctx, store.DeleteBranchMembershipsParams{ParentID: &p.ID, OrgID: p.OrgID, UserID: userID})
		return err
	})
	if err != nil {
		return err
	}
	return s.refreshAccess(ctx, p.OrgID, userID)
}

// IssueCredentials creates or rotates userID's login on p with the access
// their role allows (V2 §3.5).
func (s *Service) IssueCredentials(ctx context.Context, p store.Project, userID uuid.UUID) (provision.Credentials, error) {
	role, err := EffectiveProjectRole(ctx, store.New(s.db), p.OrgID, p.ID, userID)
	if err != nil {
		return provision.Credentials{}, err
	}
	if role == "" {
		return provision.Credentials{}, ErrNotFound
	}
	return s.projects.IssueCredentials(ctx, p, userID, authz.CredentialAccess(role))
}

// ---- Project transfer ----------------------------------------------------------

// Transfer moves p into toOrg (V2 §2.1). The caller has checked the actor
// owns both organisations. Project members come along, joining toOrg as
// members if they are not in it yet; their logins keep working.
func (s *Service) Transfer(ctx context.Context, p store.Project, toOrg uuid.UUID) error {
	if p.OrgID == toOrg {
		return fmt.Errorf("%w: the project is already in that organisation", ErrInvalid)
	}
	if p.Status != provision.StatusActive {
		return fmt.Errorf("%w: the project is %s", ErrConflict, p.Status)
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM projects WHERE id = $1 AND org_id = $2 FOR UPDATE`, p.ID, p.OrgID); err != nil {
			return err
		}
		// Org targets stay with their organisation (V2 §6): a project using
		// one, or with backups on one, moves to platform storage first.
		if uses, err := q.ProjectsUsingOrgTargets(ctx, store.ProjectsUsingOrgTargetsParams{ID: p.ID, OrgID: p.OrgID}); err != nil {
			return err
		} else if uses {
			return fmt.Errorf("%w: the project keeps backups on one of this organisation's storage targets; switch it to platform storage (copying existing backups, deleting the originals) first", ErrConflict)
		}
		members, err := q.ListProjectMembers(ctx, store.ListProjectMembersParams{ProjectID: p.ID, OrgID: p.OrgID})
		if err != nil {
			return err
		}
		for _, m := range members {
			if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: toOrg, UserID: m.UserID, Role: authz.OrgMember}); err != nil {
				return err
			}
		}
		// Every table that carries the project's org_id (members, their
		// logins, saved queries, reaped sessions, editor preferences,
		// dedicated requests) follows it by its foreign key to
		// projects (id, org_id), ON UPDATE CASCADE (migration 00068).
		if err := q.SetProjectOrg(ctx, store.SetProjectOrgParams{ID: p.ID, OrgID: p.OrgID, NewOrgID: toOrg}); err != nil {
			return err
		}
		// Favourites of the project's saved queries go too.
		if err := q.MoveProjectSavedQueryFavorites(ctx, store.MoveProjectSavedQueryFavoritesParams{ProjectID: p.ID, OrgID: p.OrgID, NewOrgID: toOrg}); err != nil {
			return err
		}
		// Tokens of the old organisation lose the project; those left
		// with no projects are revoked (V2 §2.1).
		if err := q.DropProjectFromTokens(ctx, store.DropProjectFromTokensParams{OrgID: p.OrgID, ProjectID: p.ID}); err != nil {
			return err
		}
		_, err = q.RevokeEmptiedTokens(ctx, p.OrgID)
		return err
	})
}

// ---- Invitations ------------------------------------------------------------------

// ProjectRole is one project membership in an invitation.
type ProjectRole struct {
	ProjectID uuid.UUID `json:"project_id"`
	Role      string    `json:"role"`
}

// InviteParams describes an invitation.
type InviteParams struct {
	// OrgID is nil for a platform invitation.
	OrgID     *uuid.UUID
	Email     string
	OrgRole   string
	Projects  []ProjectRole
	InvitedBy uuid.UUID
	// InviterName names the inviter in the email.
	InviterName string
}

// Invited is a new invitation and its link (shown once to the inviter, as
// the fallback when email does not arrive).
type Invited struct {
	Invitation store.Invitation
	URL        string
	// EmailError is set when the email could not be sent.
	EmailError error
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Invite records an invitation and emails it. Permission to invite with
// these roles is the caller's check.
func (s *Service) Invite(ctx context.Context, p InviteParams) (Invited, error) {
	email, err := auth.NormalizeEmail(p.Email)
	if err != nil {
		return Invited{}, err
	}
	kind := "platform"
	var orgRole *string
	orgName := ""
	q := store.New(s.db)
	if p.OrgID != nil {
		kind = "org"
		if !validOrgRole(p.OrgRole) {
			return Invited{}, fmt.Errorf("%w: role must be owner, admin, member, or billing", ErrInvalid)
		}
		orgRole = &p.OrgRole
		o, err := q.GetOrg(ctx, *p.OrgID)
		if err != nil {
			return Invited{}, err
		}
		orgName = o.Name
		if p.OrgRole == authz.OrgBilling && len(p.Projects) > 0 {
			return Invited{}, fmt.Errorf("%w: billing members have no project access", ErrInvalid)
		}
		seen := map[uuid.UUID]bool{}
		for _, pr := range p.Projects {
			if !validProjectRole(pr.Role) {
				return Invited{}, fmt.Errorf("%w: project role must be admin, developer, or read_only", ErrInvalid)
			}
			if seen[pr.ProjectID] {
				return Invited{}, fmt.Errorf("%w: a project is listed twice", ErrInvalid)
			}
			seen[pr.ProjectID] = true
			if _, err := q.GetOrgProject(ctx, store.GetOrgProjectParams{ID: pr.ProjectID, OrgID: *p.OrgID}); errors.Is(err, pgx.ErrNoRows) {
				return Invited{}, ErrNotFound
			} else if err != nil {
				return Invited{}, err
			}
		}
		if existing, err := q.GetUserByEmail(ctx, email); err == nil {
			if _, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: *p.OrgID, UserID: existing.ID}); err == nil && len(p.Projects) == 0 {
				return Invited{}, fmt.Errorf("%w: %s is already a member", ErrConflict, email)
			}
		}
	} else if len(p.Projects) > 0 {
		return Invited{}, fmt.Errorf("%w: platform invitations carry no project roles", ErrInvalid)
	} else if _, err := q.GetUserByEmail(ctx, email); err == nil {
		return Invited{}, fmt.Errorf("%w: %s already has an account", ErrConflict, email)
	}
	if p.Projects == nil {
		p.Projects = []ProjectRole{}
	}
	roles, _ := json.Marshal(p.Projects)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Invited{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	inv, err := q.InsertInvitation(ctx, store.InsertInvitationParams{
		Email: email, TokenHash: tokenDigest(token), Kind: kind, OrgID: p.OrgID, OrgRole: orgRole,
		ProjectRoles: roles, InvitedBy: p.InvitedBy, ExpiresAt: s.now().Add(InvitationTTL),
	})
	if err != nil {
		return Invited{}, err
	}
	out := Invited{Invitation: inv, URL: s.publicURL + "/invite?token=" + token}
	what := "Xenibase"
	if orgName != "" {
		what = "the " + orgName + " organisation on Xenibase"
	}
	who := p.InviterName
	if strings.TrimSpace(who) == "" {
		who = "Someone"
	}
	invite := mail.Content{
		Preheader: who + " invited you to " + what + ".",
		Title:     "You're invited to " + what,
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("%s invited you to %s. Accept the invitation to get started.", who, what)),
			mail.Button("Accept the invitation", out.URL),
			mail.P("The link works for 7 days, and only once."),
		},
		SignOff: mail.SignOff,
		Note:    "If you weren't expecting this, you can safely ignore it. Nothing happens unless you accept.",
	}
	if s.mailer == nil {
		out.EmailError = mail.ErrNotConfigured
	} else if err := s.mailer.Send(ctx, mail.Compose([]string{email}, "You're invited to "+what, invite)); err != nil {
		out.EmailError = err
		s.log.Warn("invitation email", "invitation_id", inv.ID, "err", err)
	}
	return out, nil
}

// Preview is what the invitee sees before accepting.
type Preview struct {
	Invitation  store.GetInvitationByTokenRow
	HasAccount  bool
	ProjectRole []ProjectRole
}

// PreviewToken looks up a pending invitation by its link token.
func (s *Service) PreviewToken(ctx context.Context, token string) (Preview, error) {
	q := store.New(s.db)
	inv, err := q.GetInvitationByToken(ctx, tokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, ErrNotFound
	}
	if err != nil {
		return Preview{}, err
	}
	if inv.AcceptedAt != nil || inv.RevokedAt != nil || !inv.ExpiresAt.After(s.now()) {
		return Preview{}, ErrNotFound
	}
	out := Preview{Invitation: inv}
	_ = json.Unmarshal(inv.ProjectRoles, &out.ProjectRole)
	if _, err := q.GetUserByEmail(ctx, inv.Email); err == nil {
		out.HasAccount = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Preview{}, err
	}
	return out, nil
}

// NewAccount is the account an invitee creates while accepting.
type NewAccount struct {
	Name, Password string
	TermsVersion   int
}

// AcceptToken accepts the invitation behind token. A signed-in user
// (signedIn non-nil) accepts for their own account, which must have the
// invited address; otherwise acc creates the account.
func (s *Service) AcceptToken(ctx context.Context, token string, signedIn *uuid.UUID, acc *NewAccount, ip *netip.Addr) (store.User, *uuid.UUID, error) {
	q := store.New(s.db)
	inv, err := q.GetInvitationByToken(ctx, tokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.User{}, nil, ErrNotFound
	}
	if err != nil {
		return store.User{}, nil, err
	}
	invitation := store.Invitation{ID: inv.ID, Email: inv.Email, Kind: inv.Kind, OrgID: inv.OrgID, OrgRole: inv.OrgRole,
		ProjectRoles: inv.ProjectRoles, ExpiresAt: inv.ExpiresAt, AcceptedAt: inv.AcceptedAt, RevokedAt: inv.RevokedAt}
	return s.accept(ctx, invitation, signedIn, acc, ip)
}

// AcceptID accepts one of a signed-in user's pending invitations.
func (s *Service) AcceptID(ctx context.Context, id, userID uuid.UUID) (*uuid.UUID, error) {
	inv, err := store.New(s.db).GetInvitation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, org, err := s.accept(ctx, inv, &userID, nil, nil)
	return org, err
}

func (s *Service) accept(ctx context.Context, inv store.Invitation, signedIn *uuid.UUID, acc *NewAccount, ip *netip.Addr) (store.User, *uuid.UUID, error) {
	if inv.AcceptedAt != nil || inv.RevokedAt != nil || !inv.ExpiresAt.After(s.now()) {
		return store.User{}, nil, ErrNotFound
	}
	var roles []ProjectRole
	_ = json.Unmarshal(inv.ProjectRoles, &roles)
	var u store.User
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		existing, err := q.GetUserByEmail(ctx, inv.Email)
		switch {
		case err == nil:
			if signedIn == nil {
				return ErrSignInFirst
			}
			if *signedIn != existing.ID {
				return ErrWrongAccount
			}
			u = existing
		case errors.Is(err, pgx.ErrNoRows):
			if signedIn != nil {
				return ErrWrongAccount
			}
			if acc == nil {
				return fmt.Errorf("%w: choose a password to create your account", ErrInvalid)
			}
			if u, err = s.auth.CreateVerifiedUser(ctx, tx, inv.Email, acc.Password, acc.Name, acc.TermsVersion, ip); err != nil {
				return err
			}
		default:
			return err
		}
		n, err := q.MarkInvitationAccepted(ctx, store.MarkInvitationAcceptedParams{ID: inv.ID, UserID: &u.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound // accepted or revoked meanwhile
		}
		if inv.Kind != "org" || inv.OrgID == nil {
			return nil
		}
		if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: *inv.OrgID, UserID: u.ID, Role: *inv.OrgRole}); err != nil {
			return err
		}
		for _, pr := range roles {
			p, err := q.GetOrgProject(ctx, store.GetOrgProjectParams{ID: pr.ProjectID, OrgID: *inv.OrgID})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.DeletedAt != nil) {
				continue // the project went away (or left the org) since
			}
			if err != nil {
				return err
			}
			if err := q.UpsertProjectMember(ctx, store.UpsertProjectMemberParams{
				ProjectID: p.ID, UserID: u.ID, OrgID: *inv.OrgID, Role: pr.Role, AddedBy: nil,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return store.User{}, nil, fmt.Errorf("%w: an account with this address was just created; sign in to accept", ErrConflict)
	}
	if err != nil {
		return store.User{}, nil, err
	}
	if inv.OrgID != nil {
		if err := s.refreshAccess(ctx, *inv.OrgID, u.ID); err != nil {
			s.log.Warn("refresh access after accepting", "err", err)
		}
	}
	return u, inv.OrgID, nil
}
