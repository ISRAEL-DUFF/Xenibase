// Package authz decides who may do what (V2 §2.3–2.6). Every tenant
// resource resolves to an organisation before a check; Can is the single
// entry point the API uses.
//
// A decision has two parts. Visible says whether the actor may know the
// resource exists: resources in organisations (or projects) the actor
// cannot see answer 404, never 403, so their existence does not leak.
// Allowed says whether the action itself is permitted.
package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// Actor kinds (V2 §2.5).
const (
	ActorSession = "session"
	ActorToken   = "token"
	ActorSystem  = "system"
)

// Actor is who makes a request.
type Actor struct {
	Kind          string
	UserID        uuid.UUID
	PlatformAdmin bool
	// Support is support staff (V3 §7.1).
	Support bool
	// TokenID and TokenOrg are set for API tokens (V2 §7.2), which act
	// only in their organisation, within their scopes, and (when
	// TokenProjects is not nil) only on those projects.
	TokenID       *uuid.UUID
	TokenOrg      *uuid.UUID
	TokenScopes   []string
	TokenProjects []uuid.UUID
}

// Token scopes (V2 §7.2).
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
)

// actionScope is the token scope each action needs: read to view, write to
// create and modify, admin for destructive and settings actions.
var actionScope = map[Action]string{
	OrgView:            ScopeRead,
	OrgAudit:           ScopeRead,
	ProjectView:        ScopeRead,
	ProjectCredentials: ScopeRead,
	ConsoleRead:        ScopeRead,
	ProjectAudit:       ScopeRead,
	ProjectExport:      ScopeAdmin,
	ProjectResidency:   ScopeAdmin,
	ProjectManageRoles: ScopeAdmin,
	ConsoleWrite:       ScopeWrite,
	TableEdit:          ScopeWrite,
	BackupCreate:       ScopeWrite,
	OrgCreateProject:   ScopeWrite,
	OrgManage:          ScopeAdmin,
	OrgOwnerOnly:       ScopeAdmin,
	OrgBillingManage:   ScopeAdmin,
	RestoreInPlace:     ScopeAdmin,
	BackupStorage:      ScopeAdmin,
	BranchManage:       ScopeWrite,
	AutomationManage:   ScopeWrite,
	ProjectSettings:    ScopeAdmin,
	ServicesManage:     ScopeAdmin,
	ServicesLogs:       ScopeRead,
	AuthUsers:          ScopeRead,
	AuthUsersManage:    ScopeWrite,
	FilesView:          ScopeRead,
	FilesManage:        ScopeWrite,
	RealtimeView:       ScopeRead,
	RealtimeManage:     ScopeWrite,
	ProjectMembers:     ScopeAdmin,
	ProjectPromote:     ScopeAdmin,
	ProjectDelete:      ScopeAdmin,
}

// ScopeFor is the token scope action needs.
func ScopeFor(a Action) string {
	if s, ok := actionScope[a]; ok {
		return s
	}
	return ScopeAdmin
}

// HasScope reports whether scopes grant want (write includes read, admin
// includes both).
func HasScope(scopes []string, want string) bool {
	rank := map[string]int{ScopeRead: 1, ScopeWrite: 2, ScopeAdmin: 3}
	for _, s := range scopes {
		if rank[s] >= rank[want] {
			return true
		}
	}
	return false
}

// AllowsProject reports whether a token actor may touch project id.
func (a Actor) AllowsProject(id uuid.UUID) bool {
	if a.Kind != ActorToken || a.TokenProjects == nil {
		return true
	}
	for _, p := range a.TokenProjects {
		if p == id {
			return true
		}
	}
	return false
}

// Restricted reports whether the actor is a project-restricted token.
func (a Actor) Restricted() bool { return a.Kind == ActorToken && a.TokenProjects != nil }

// Action is something an actor wants to do.
type Action string

// Organisation roles.
const (
	OrgOwner  = "owner"
	OrgAdmin  = "admin"
	OrgMember = "member"
	// OrgBilling sees and manages billing, without project access (V3 §3.2).
	OrgBilling = "billing"
)

// Project roles.
const (
	ProjectAdmin     = "admin"
	ProjectDeveloper = "developer"
	ProjectReadOnly  = "read_only"
)

// Actions. The comment on each is the least role that may perform it.
const (
	// Signed-in users acting on themselves (their account, their orgs list,
	// creating an org, their invitations).
	Self Action = "self"

	// Platform admin (V2 §2.4): nodes, platform settings and storage,
	// signup, users, platform invitations, isolation checks, alerts,
	// platform audit, /metrics.
	PlatformManage Action = "platform.manage"
	// SupportConsole is support staff and platform admins (V3 §7.1): the
	// ticket console and organisations' metadata, never tenant data.
	SupportConsole Action = "support.console"

	OrgView          Action = "org.view"           // member
	OrgCreateProject Action = "org.create_project" // admin; member when the org allows it
	OrgManage        Action = "org.manage"         // admin: settings, members, invitations
	OrgAudit         Action = "org.audit"          // admin: org audit log, usage
	OrgOwnerOnly     Action = "org.owner"          // owner: owners, delete, transfer projects out
	// OrgBillingManage is billing: owners and billing members only (V3 §3.2).
	OrgBillingManage Action = "org.billing"

	ProjectView        Action = "project.view"         // read_only: project, metrics, operations
	ProjectCredentials Action = "project.credentials"  // read_only (read-only credentials)
	ConsoleRead        Action = "project.console_read" // read_only
	ConsoleWrite       Action = "project.console_write"
	TableEdit          Action = "project.table_edit"      // developer: the table editor's rows and schema (V2 §4)
	BackupCreate       Action = "project.backup"          // developer: back up, restore into a new project
	RestoreInPlace     Action = "project.restore_inplace" // admin
	BackupStorage      Action = "project.backup_storage"  // admin: storage target, backup key and its download (V2 s6)
	BranchManage       Action = "project.branch"          // developer: create, reset, detach and delete branches (V2 s8)
	AutomationManage   Action = "project.automation"      // developer: webhooks and scheduled jobs (V2 s9)
	ProjectSettings    Action = "project.settings"        // admin: rotate, settings, extensions, PITR
	ProjectMembers     Action = "project.members"         // admin
	ProjectPromote     Action = "project.promote"         // admin
	ProjectDelete      Action = "project.delete"          // admin
	ProjectAudit       Action = "project.audit"           // admin
	// ServicesManage turns backend services on and off and manages their
	// API keys and settings (V4 §2.2, §2.4); ServicesLogs reads the API
	// request logs.
	ServicesManage Action = "project.services"      // admin
	ServicesLogs   Action = "project.services_logs" // developer
	// AuthUsers reads the project's app users (V4 §4.9); AuthUsersManage
	// invites, bans, deletes and signs them out. Their auth settings are
	// ServicesManage.
	AuthUsers       Action = "project.auth_users"        // developer
	AuthUsersManage Action = "project.auth_users_manage" // developer
	// FilesView browses and downloads the project's stored files (V4 §5);
	// FilesManage uploads, deletes and changes buckets.
	FilesView   Action = "project.files"        // developer
	FilesManage Action = "project.files_manage" // developer
	// RealtimeView shows realtime's tables, limits and use (V4 §6);
	// RealtimeManage turns it on for tables and sets history topics.
	RealtimeView   Action = "project.realtime"        // developer
	RealtimeManage Action = "project.realtime_manage" // developer
	// ProjectExport downloads a project's data (a backup as a pg_dump
	// archive, V2 §10.10): organisation owners only, and like any project
	// action invisible to those who can't see the project.
	ProjectExport Action = "project.export"
	// ProjectResidency turns a project's data residency on or off (V3
	// §6.3): organisation owners only.
	ProjectResidency Action = "project.residency"
	// ProjectManageRoles turns role management (the owner's CREATEROLE) on
	// or off for a dedicated project: organisation owners only.
	ProjectManageRoles Action = "project.manage_roles"
)

// projectMin is the least project role for each project action.
var projectMin = map[Action]string{
	ProjectView:        ProjectReadOnly,
	ProjectCredentials: ProjectReadOnly,
	ConsoleRead:        ProjectReadOnly,
	ConsoleWrite:       ProjectDeveloper,
	TableEdit:          ProjectDeveloper,
	BackupCreate:       ProjectDeveloper,
	RestoreInPlace:     ProjectAdmin,
	BackupStorage:      ProjectAdmin,
	BranchManage:       ProjectDeveloper,
	AutomationManage:   ProjectDeveloper,
	ProjectSettings:    ProjectAdmin,
	ServicesManage:     ProjectAdmin,
	ServicesLogs:       ProjectDeveloper,
	AuthUsers:          ProjectDeveloper,
	AuthUsersManage:    ProjectDeveloper,
	FilesView:          ProjectDeveloper,
	FilesManage:        ProjectDeveloper,
	RealtimeView:       ProjectDeveloper,
	RealtimeManage:     ProjectDeveloper,
	ProjectMembers:     ProjectAdmin,
	ProjectPromote:     ProjectAdmin,
	ProjectDelete:      ProjectAdmin,
	ProjectAudit:       ProjectAdmin,
	ProjectExport:      ProjectReadOnly, // visibility; owners only, below
	ProjectResidency:   ProjectReadOnly, // visibility; owners only, below
	ProjectManageRoles: ProjectReadOnly, // visibility; owners only, below
}

// IsProjectAction reports whether a is checked against a project.
func IsProjectAction(a Action) bool { _, ok := projectMin[a]; return ok }

var projectRank = map[string]int{ProjectReadOnly: 1, ProjectDeveloper: 2, ProjectAdmin: 3}

var orgRank = map[string]int{OrgMember: 1, OrgAdmin: 2, OrgOwner: 3}

// Resource is what an action applies to. ProjectID implies OrgID.
type Resource struct {
	OrgID     uuid.UUID
	ProjectID uuid.UUID
	// ParentID is a branch's parent: a token restricted to the parent
	// covers its branches (V2 §8: a CI token creates and uses them).
	ParentID uuid.UUID
}

// Decision is the outcome of a check, with the roles it was based on.
type Decision struct {
	Visible     bool
	Allowed     bool
	OrgRole     string // "" when not a member
	ProjectRole string // effective: org owners and admins are project admins
	// BreakGlass is set when a platform admin acts through a break-glass
	// session (V2 §2.4): every such action is flagged in the audit logs.
	BreakGlass bool
	// Frozen is set when the organisation is suspended or being deleted
	// and the action is not a read (V2 §10.8): Allowed is false.
	Frozen bool
	// NeedScope is set when an API token's user may do this but the token
	// lacks the scope (or its project restriction rules out org actions).
	NeedScope string
}

// Queries is the subset of store the checks need.
type Queries interface {
	GetOrgMember(ctx context.Context, arg store.GetOrgMemberParams) (store.OrgMember, error)
	GetProjectMember(ctx context.Context, arg store.GetProjectMemberParams) (store.ProjectMember, error)
	GetOrg(ctx context.Context, orgID uuid.UUID) (store.Organization, error)
	ActiveBreakGlass(ctx context.Context, arg store.ActiveBreakGlassParams) (store.BreakGlassSession, error)
}

// readActions still work in a suspended organisation, so its members can
// see what happened (V2 §10.8).
var readActions = map[Action]bool{OrgView: true, OrgAudit: true, ProjectView: true, ProjectAudit: true}

// Can decides whether actor may perform action on res.
func Can(ctx context.Context, q Queries, actor Actor, action Action, res Resource) (Decision, error) {
	switch action {
	case Self:
		ok := actor.Kind == ActorSession
		return Decision{Visible: true, Allowed: ok}, nil
	case PlatformManage:
		ok := actor.PlatformAdmin && actor.Kind == ActorSession
		// Platform routes are not tenant resources: refusing them is 403.
		return Decision{Visible: true, Allowed: ok}, nil
	case SupportConsole:
		ok := (actor.PlatformAdmin || actor.Support) && actor.Kind == ActorSession
		return Decision{Visible: true, Allowed: ok}, nil
	}
	if actor.Kind == ActorSystem {
		return Decision{Visible: true, Allowed: true, OrgRole: OrgOwner, ProjectRole: ProjectAdmin}, nil
	}
	if res.OrgID == uuid.Nil {
		return Decision{}, nil
	}
	token := actor.Kind == ActorToken
	if token && (actor.TokenOrg == nil || *actor.TokenOrg != res.OrgID) {
		return Decision{}, nil // a token acts only in its own organisation
	}
	if token && res.ProjectID != uuid.Nil && !actor.AllowsProject(res.ProjectID) &&
		(res.ParentID == uuid.Nil || !actor.AllowsProject(res.ParentID)) {
		return Decision{}, nil // nor outside its projects
	}
	d, err := can(ctx, q, actor, action, res)
	if err != nil || !d.Allowed {
		return d, err
	}
	if token {
		switch need := ScopeFor(action); {
		case !HasScope(actor.TokenScopes, need):
			d.Allowed, d.NeedScope = false, need
			return d, nil
		case actor.Restricted() && res.ProjectID == uuid.Nil && action != OrgView:
			// A project-restricted token can't act on the organisation.
			d.Allowed, d.NeedScope = false, "unrestricted"
			return d, nil
		}
	}
	if readActions[action] {
		return d, nil
	}
	o, err := q.GetOrg(ctx, res.OrgID)
	if err != nil {
		return d, err
	}
	switch o.Status {
	case "suspended":
		d.Allowed, d.Frozen = false, true
	case "deleting":
		// Only an owner cancelling the deletion.
		if action != OrgOwnerOnly {
			d.Allowed, d.Frozen = false, true
		}
	}
	return d, nil
}

func can(ctx context.Context, q Queries, actor Actor, action Action, res Resource) (Decision, error) {
	var d Decision
	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{OrgID: res.OrgID, UserID: actor.UserID})
	switch {
	case err == nil:
		d.OrgRole = m.Role
	case !errors.Is(err, pgx.ErrNoRows):
		return d, err
	case actor.PlatformAdmin && actor.Kind == ActorSession:
		// Not a member: a platform admin with an open break-glass session
		// acts as an org admin (V2 §2.4).
		_, err := q.ActiveBreakGlass(ctx, store.ActiveBreakGlassParams{OrgID: res.OrgID, AdminID: actor.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return d, nil
		}
		if err != nil {
			return d, err
		}
		d.OrgRole, d.BreakGlass = OrgAdmin, true
	default:
		return d, nil
	}

	if minRole, ok := projectMin[action]; ok {
		if res.ProjectID == uuid.Nil {
			return Decision{}, errors.New("authz: project action without a project")
		}
		if d.OrgRole == OrgBilling {
			return Decision{OrgRole: d.OrgRole}, nil // no project access
		}
		if d.OrgRole == OrgOwner || d.OrgRole == OrgAdmin {
			d.ProjectRole = ProjectAdmin
		} else {
			pm, err := q.GetProjectMember(ctx, store.GetProjectMemberParams{ProjectID: res.ProjectID, UserID: actor.UserID, OrgID: res.OrgID})
			if errors.Is(err, pgx.ErrNoRows) {
				return Decision{OrgRole: d.OrgRole, BreakGlass: d.BreakGlass}, nil // a member who isn't on the project can't see it
			}
			if err != nil {
				return d, err
			}
			d.ProjectRole = pm.Role
		}
		d.Visible = true
		d.Allowed = projectRank[d.ProjectRole] >= projectRank[minRole]
		if action == ProjectExport || action == ProjectResidency || action == ProjectManageRoles {
			d.Allowed = d.OrgRole == OrgOwner && !d.BreakGlass
		}
		return d, nil
	}

	d.Visible = true
	switch action {
	case OrgView:
		d.Allowed = true
	case OrgManage, OrgAudit:
		d.Allowed = orgRank[d.OrgRole] >= orgRank[OrgAdmin]
	case OrgOwnerOnly:
		d.Allowed = d.OrgRole == OrgOwner
	case OrgBillingManage:
		d.Allowed = (d.OrgRole == OrgOwner || d.OrgRole == OrgBilling) && !d.BreakGlass
	case OrgCreateProject:
		if d.OrgRole == OrgBilling {
			break
		}
		if orgRank[d.OrgRole] >= orgRank[OrgAdmin] {
			d.Allowed = true
			break
		}
		o, err := q.GetOrg(ctx, res.OrgID)
		if err != nil {
			return d, err
		}
		s, err := store.DecodeOrgSettings(o.Settings)
		if err != nil {
			return d, err
		}
		d.Allowed = s.MembersCanCreateProjects
	default:
		return Decision{}, errors.New("authz: unknown action " + string(action))
	}
	return d, nil
}

// CredentialAccess is the database access a project role's personal
// credentials get (V2 §3.5): read/write for developers and admins.
func CredentialAccess(projectRole string) string {
	if projectRank[projectRole] >= projectRank[ProjectDeveloper] {
		return "read_write"
	}
	return "read_only"
}

// AtLeast reports whether project role have is at least want.
func AtLeast(have, want string) bool { return projectRank[have] >= projectRank[want] }
