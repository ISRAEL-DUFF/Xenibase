package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// Only a platform admin chooses the node a dedicated project goes on; an
// organisation owner who names one is refused before anything is placed.
func TestCreateProjectNodeIsForPlatformAdmins(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	q := store.New(pool)
	u, err := q.InsertUser(ctx, store.InsertUserParams{Email: "owner@example.com", PasswordHash: "x", PlatformRole: "user"})
	if err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'o', 'o', id FROM quota_plans WHERE name = 'Personal' RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: org, UserID: u.ID, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: pool, log: log, projects: provision.NewService(pool, kr, nil, provision.Config{}, log)}

	create := func(role string) (int, string) {
		body := `{"org_id": "` + org.String() + `", "name": "x", "tier": "dedicated", "node_id": "` + uuid.NewString() + `"}`
		r := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), keySession, auth.Session{UserID: u.ID, PlatformRole: role}))
		w := httptest.NewRecorder()
		s.CreateProject(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := create("user"); code != http.StatusForbidden || !strings.Contains(body, "node") {
		t.Fatalf("an organisation owner chose the node: %d %s", code, body)
	}
	// A platform admin gets past the check (to a node that does not exist).
	if code, body := create(auth.RolePlatformAdmin); code == http.StatusForbidden {
		t.Fatalf("a platform admin was refused: %d %s", code, body)
	}
}
