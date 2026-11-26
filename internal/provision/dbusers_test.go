package provision

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

func (h *harness) personalRoles(t *testing.T, db string) []string {
	t.Helper()
	rows, err := h.cluster.Query(context.Background(), `SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)`, db+"_u_")
	if err != nil {
		t.Fatal(err)
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return roles
}

func activate(t *testing.T, h *harness, p store.Project) store.Project {
	t.Helper()
	if _, err := h.db.Exec(context.Background(), `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	p.Status = StatusActive
	return p
}

// A personal login whose metadata row cannot be written does not stay on
// the instance with a password no one has.
func TestIssueCredentialsLeavesNoRoleOnFailure(t *testing.T) {
	h := newHarness(t)
	p, _ := h.create(t)
	p = activate(t, h, p)
	// No such user: the metadata write fails after the role was made.
	if _, err := h.s.IssueCredentials(context.Background(), p, uuid.New(), AccessReadWrite); err == nil {
		t.Fatal("issued credentials to a user that does not exist")
	}
	if roles := h.personalRoles(t, p.DbName); len(roles) != 0 {
		t.Fatalf("the instance kept %v", roles)
	}
}

// Teardown drops personal logins the metadata does not know about.
func TestTeardownDropsUnrecordedPersonalLogins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.create(t)
	stray := p.DbName + "_u_abc123"
	if _, err := h.cluster.Exec(ctx, "CREATE ROLE "+ident(stray)+" LOGIN IN ROLE "+ident(p.OwnerRole)); err != nil {
		t.Fatal(err)
	}
	if err := h.s.teardown(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	if roles := h.personalRoles(t, p.DbName); len(roles) != 0 {
		t.Fatalf("teardown left %v", roles)
	}
}
