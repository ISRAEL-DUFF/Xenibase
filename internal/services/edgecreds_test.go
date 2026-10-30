package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/edgecred"
	"github.com/israel-duff/xenibase/internal/signature"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestEdgeScope: an edge's own credential sees only its region, whatever
// region it asks for, and the platform-wide secret only the regions
// without their own API domain (every region only when configured).
func TestEdgeScope(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	const global = "platform-wide-edge-secret-0123456789abcdef"
	s := &Service{db: db, keyring: kr, log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{
		EdgeSecret:   global,
		RegionDomain: func(r string) string { return map[string]string{"ng-lagos": "ng.api.example.com"}[r] },
	}}
	if _, err := db.Exec(ctx, `INSERT INTO regions (id, name, api_domain) VALUES ('ng-lagos', 'Lagos', 'ng.api.example.com')`); err != nil {
		t.Fatal(err)
	}
	// A project in each region.
	refs := map[string]string{"eu-central": "euprojec", "ng-lagos": "ngprojec"}
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		for region, ref := range refs {
			id := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
				VALUES ($1, $2, $2, $2, $2, 'x', 'shared', gen_random_uuid(), 'active', gen_random_uuid(), $3)`, id, ref, region); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO project_services (project_id, ref) VALUES ($1, $2)`, id, ref); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cred, err := edgecred.Issue(ctx, db, kr, "edge-ng-1", "ng-lagos", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := edgecred.Issue(ctx, db, kr, "edge-ng-1", "ng-lagos", nil); err != nil || again != cred {
		t.Fatalf("issuing again for the same edge: %v", err)
	}
	if _, err := edgecred.Issue(ctx, db, kr, "edge-ng-1", "eu-central", nil); !errors.Is(err, edgecred.ErrConflict) {
		t.Fatalf("a second region for one edge: %v", err)
	}

	verify := func(secret string) (EdgeScope, error) {
		r := httptest.NewRequest(http.MethodGet, edgeapi.PathConfig+"?since=0", nil)
		if id, ok := edgeapi.CredentialID(secret); ok {
			r.Header.Set(edgeapi.HeaderCredential, id.String())
		}
		r.Header.Set(signature.Header, signature.Sign(secret, time.Now(), edgeapi.Signed(r.Method, r.URL.RequestURI(), nil)))
		return s.VerifyEdge(r, nil)
	}
	check := func(name string, sc EdgeScope, sees map[string]bool) {
		t.Helper()
		allow, err := sc.Feed("")
		if err != nil {
			t.Fatal(err)
		}
		for region, want := range sees {
			if allow(region) != want {
				t.Errorf("%s: sees %s = %v, want %v", name, region, !want, want)
			}
			if _, err := sc.Feed(region); (err == nil) != want {
				t.Errorf("%s: asking for %s: %v", name, region, err)
			}
			if err := s.EdgeProjectInScope(ctx, sc, refs[region]); (err == nil) != want {
				t.Errorf("%s: acting for a project in %s: %v", name, region, err)
			}
		}
	}

	sc, err := verify(cred)
	if err != nil {
		t.Fatal(err)
	}
	check("credential", sc, map[string]bool{"ng-lagos": true, "eu-central": false})

	sc, err = verify(global)
	if err != nil {
		t.Fatal(err)
	}
	check("platform-wide secret", sc, map[string]bool{"ng-lagos": false, "eu-central": true})

	s.cfg.EdgeSecretAllRegions = true
	sc, _ = verify(global)
	check("platform-wide secret, all regions", sc, map[string]bool{"ng-lagos": true, "eu-central": true})

	// Signed with another secret under the credential's id, or revoked.
	id, _ := edgeapi.CredentialID(cred)
	forged, _ := edgeapi.NewCredential(id)
	if _, err := verify(forged); err == nil {
		t.Fatal("a forged credential verified")
	}
	if err := edgecred.Revoke(ctx, db, "edge-ng-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(cred); err == nil {
		t.Fatal("a revoked credential verified")
	}
}
