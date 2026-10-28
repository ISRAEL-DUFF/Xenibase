package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/regions"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestRegionAPIDomainAndEarlyAccess is the Lagos plan's two gaps: a hidden
// region takes projects from its early-access organisations only (a
// rehearsal, a first customer), and a region's own API domain gives its
// projects an API URL its own edge serves, so their traffic stays there.
func TestRegionAPIDomainAndEarlyAccess(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	ctx := context.Background()
	const domain = "ng.api.xenibase.test"
	if _, err := e.Regions.Save(ctx, regions.Input{ID: "ng-early", Name: "Lagos early", Country: "NG", Hidden: true}); err != nil {
		t.Fatal(err)
	}
	// The test node serves the region, so its projects have somewhere to go.
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET region = 'ng-early'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.DB.Exec(bg, `UPDATE nodes SET region = 'eu-central'`)
		_, _ = e.DB.Exec(bg, `UPDATE projects SET region = 'eu-central'`)
		_, _ = e.DB.Exec(bg, `DELETE FROM regions WHERE id = 'ng-early'`)
		_ = e.Regions.Load(bg)
	})
	put := func(req gen.AdminRegionRequest) (int, gen.AdminRegion, gen.Error) {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		code, raw := e.DoBytes("PUT", "/api/v1/admin/regions/ng-early", "application/json", b)
		var out gen.AdminRegion
		var er gen.Error
		_ = json.Unmarshal(raw, &out)
		_ = json.Unmarshal(raw, &er)
		return code, out, er
	}
	base := gen.AdminRegionRequest{Name: "Lagos early", Country: ptr("NG"), Hidden: ptr(true)}
	listed := func() *gen.Region {
		t.Helper()
		var rl gen.RegionList
		if code := e.Do("GET", "/api/v1/regions", nil, &rl); code != http.StatusOK {
			t.Fatalf("regions: %d", code)
		}
		for _, r := range rl.Items {
			if r.Id == "ng-early" {
				return &r
			}
		}
		return nil
	}
	create := func(name string, org uuid.UUID) (int, gen.ProjectCredentials) {
		t.Helper()
		var c gen.ProjectCredentials
		code := e.Do("POST", "/api/v1/projects", map[string]any{"name": name, "org_id": org, "region": "ng-early"}, &c)
		return code, c
	}

	// ---- The API domain is checked --------------------------------------------------------
	for _, bad := range []string{"not a domain", "localhost", testenv.EdgeDomain} {
		req := base
		req.ApiDomain = ptr(bad)
		if code, _, er := put(req); code != http.StatusBadRequest {
			t.Fatalf("api domain %q: %d %+v", bad, code, er)
		}
	}
	if _, err := e.Regions.Save(ctx, regions.Input{ID: "za-other", Name: "Other", APIDomain: "za.api.xenibase.test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.DB.Exec(context.Background(), `DELETE FROM regions WHERE id = 'za-other'`) })
	if req := base; true {
		req.ApiDomain = ptr("ZA.api.xenibase.test.")
		if code, _, er := put(req); code != http.StatusBadRequest || !strings.Contains(er.Message, "already uses") {
			t.Fatalf("another region's API domain: %d %+v", code, er)
		}
	}

	// ---- Hidden, without early access: neither listed nor usable ---------------------------
	req := base
	req.ApiDomain = ptr(domain)
	if code, out, er := put(req); code != http.StatusOK || out.ApiDomain != domain || len(out.EarlyAccessOrgIds) != 0 {
		t.Fatalf("save the domain: %d %+v %+v", code, out, er)
	}
	if r := listed(); r != nil {
		t.Fatalf("a hidden region is listed: %+v", r)
	}
	if code, _ := create("too-early", e.OrgID); code != http.StatusBadRequest {
		t.Fatalf("a project in a hidden region: %d", code)
	}

	// ---- Early access for the owner's organisation ----------------------------------------
	req.EarlyAccessOrgIds = &[]uuid.UUID{e.OrgID}
	if code, out, er := put(req); code != http.StatusOK || !slices.Equal(out.EarlyAccessOrgIds, []uuid.UUID{e.OrgID}) {
		t.Fatalf("early access: %d %+v %+v", code, out, er)
	}
	// Left out, the list is kept.
	if code, out, _ := put(gen.AdminRegionRequest{Name: "Lagos early", Country: ptr("NG"), Hidden: ptr(true), ApiDomain: ptr(domain)}); code != http.StatusOK || len(out.EarlyAccessOrgIds) != 1 {
		t.Fatalf("a save without the list: %d %+v", code, out)
	}
	if r := listed(); r == nil || r.EarlyAccessOrgIds == nil || !slices.Equal(*r.EarlyAccessOrgIds, []uuid.UUID{e.OrgID}) {
		t.Fatalf("listed for early access: %+v", r)
	}
	if req := (gen.AdminRegionRequest{Name: "x", EarlyAccessOrgIds: &[]uuid.UUID{uuid.New()}}); true {
		if code, _, _ := put(req); code != http.StatusBadRequest {
			t.Fatalf("early access for no organisation: %d", code)
		}
	}
	other := e.CreateOrg("Not early")
	if code, _ := create("other-org", other); code != http.StatusBadRequest {
		t.Fatalf("an organisation without early access: %d", code)
	}
	code, c := create("rehearsal", e.OrgID)
	if code != http.StatusAccepted {
		t.Fatalf("an early-access project: %d", code)
	}
	if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	if c.Project.Region == nil || *c.Project.Region != "ng-early" {
		t.Fatalf("the project's region: %v", c.Project.Region)
	}

	// ---- Its API is at <ref>.<the region's domain>, served by the region's edge ------------
	pid := c.Project.Id.String()
	var en gen.BackendServicesEnabled
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/services", nil, &en); code != http.StatusAccepted {
		t.Fatalf("enable: %d", code)
	}
	if op := e.WaitOperation(en.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	ref := *en.Services.Ref
	if en.Services.Url == nil || *en.Services.Url != "https://"+ref+"."+domain {
		t.Fatalf("the services URL: %v", en.Services.Url)
	}
	var svc gen.BackendServices
	if code := e.Do("GET", "/api/v1/projects/"+pid+"/services", nil, &svc); code != http.StatusOK || svc.Url == nil || *svc.Url != "https://"+ref+"."+domain {
		t.Fatalf("GET services: %d %v", code, svc.Url)
	}
	var pub string
	for _, k := range en.Keys {
		if k.Key.Kind == gen.ApiKeyKindPublishable {
			pub = k.Value
		}
	}
	lagos := e.StartEdge(func(c *edge.Config) { c.Name, c.Domain, c.Region = "edge-lagos", domain, "ng-early" })
	if code, _, body := lagos.Do(ref, "GET", "/data/v1/health", nil, "apikey", pub); code != http.StatusOK {
		t.Fatalf("the Lagos edge at %s.%s: %d %s", ref, domain, code, body)
	}
	// The home region's edge doesn't serve it.
	home := e.StartEdge(func(c *edge.Config) { c.Name, c.Region = "edge-home", "eu-central" })
	if code, _, _ := home.Do(ref, "GET", "/data/v1/health", nil, "apikey", pub); code == http.StatusOK {
		t.Fatal("the home region's edge serves a Lagos project")
	}
}
