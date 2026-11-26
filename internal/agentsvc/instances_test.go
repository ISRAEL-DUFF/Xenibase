package agentsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/docker"
)

// fakeContainer is a container as fakeDocker keeps it.
type fakeContainer struct {
	ID, Name, Image, ImageID string
	Running                  bool
	Status                   string
}

// fakeDocker is the part of the Engine API instances.Create uses, with
// containers in memory. A new container made from badImage fails as
// failMode says: "create" (refused) or "start" (exits at once).
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer // by name
	images     map[string]string         // tag or ID → image ID
	versions   map[string]string         // image ID → PG_VERSION
	badImage   string
	failMode   string
	n          int
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/version" {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.43", "MinAPIVersion": "1.12"})
		return
	}
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "no such object"})
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case parts[0] == "images" && len(parts) == 3:
		id, ok := f.images[parts[1]]
		if !ok {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Env": []string{"PG_VERSION=" + f.versions[id]}}})
	case path == "/volumes/create":
		w.WriteHeader(http.StatusCreated)
	case path == "/containers/create":
		var cfg docker.ContainerConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		name := r.URL.Query().Get("name")
		if _, ok := f.containers[name]; ok {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "name in use"})
			return
		}
		id := f.images[cfg.Image]
		if id == f.badImage && f.failMode == "create" {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "create refused"})
			return
		}
		f.n++
		c := &fakeContainer{ID: "c" + strings.Repeat("x", f.n), Name: name, Image: cfg.Image, ImageID: id, Status: "created"}
		f.containers[name] = c
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": c.ID})
	case parts[0] == "containers" && len(parts) >= 2:
		c := f.containers[parts[1]]
		if c == nil {
			for _, x := range f.containers {
				if x.ID == parts[1] {
					c = x
				}
			}
		}
		if c == nil {
			notFound()
			return
		}
		action := ""
		if len(parts) > 2 {
			action = parts[2]
		}
		switch {
		case r.Method == http.MethodDelete:
			delete(f.containers, c.Name)
			w.WriteHeader(http.StatusNoContent)
		case action == "json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": c.ID, "Image": c.ImageID, "Name": "/" + c.Name,
				"State":  map[string]any{"Status": c.Status, "Running": c.Running, "ExitCode": map[bool]int{true: 0, false: 1}[c.Running]},
				"Config": map[string]any{"Image": c.Image, "Labels": map[string]string{}},
			})
		case action == "start":
			if c.ImageID == f.badImage && f.failMode == "start" {
				c.Running, c.Status = false, "exited"
			} else {
				c.Running, c.Status = true, "running"
			}
			w.WriteHeader(http.StatusNoContent)
		case action == "stop":
			c.Running, c.Status = false, "exited"
			w.WriteHeader(http.StatusNoContent)
		case action == "rename":
			to := r.URL.Query().Get("name")
			if _, ok := f.containers[to]; ok {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "name in use"})
				return
			}
			delete(f.containers, c.Name)
			c.Name = to
			f.containers[to] = c
			w.WriteHeader(http.StatusNoContent)
		case action == "logs":
			w.WriteHeader(http.StatusOK)
		case action == "exec":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "e1"})
		default:
			notFound()
		}
	case parts[0] == "exec" && len(parts) == 3 && parts[2] == "start":
		w.WriteHeader(http.StatusOK)
	case parts[0] == "exec" && len(parts) == 3 && parts[2] == "json":
		_ = json.NewEncoder(w).Encode(map[string]any{"ExitCode": 0})
	default:
		notFound()
	}
}

const testInstance = "0b6f3c1e-2a4d-4e8f-9a0b-1c2d3e4f5a6b"

// newFakeInstances is an instance on release 18.1 whose image tag now
// holds 18.2.
func newFakeInstances(t *testing.T, failMode string) (*instances, *fakeDocker) {
	t.Helper()
	f := &fakeDocker{
		containers: map[string]*fakeContainer{},
		images:     map[string]string{"xenibase-postgres:18": "sha256:new", "sha256:new": "sha256:new", "sha256:old": "sha256:old"},
		versions:   map[string]string{"sha256:new": "18.2-1.pgdg13+1", "sha256:old": "18.1-1.pgdg13+1"},
		badImage:   "sha256:new", failMode: failMode,
	}
	name, _, _ := names(testInstance)
	f.containers[name] = &fakeContainer{ID: "old", Name: name, Image: "xenibase-postgres:18", ImageID: "sha256:old", Running: true, Status: "running"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dc, err := docker.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	in := &instances{cfg: InstanceConfig{Image: "xenibase-postgres:{major}"}, dc: dc, locks: map[string]*sync.Mutex{}}
	return in, f
}

func recreateSpec() agentapi.InstanceSpec {
	return agentapi.InstanceSpec{ID: testInstance, Kind: agentapi.InstanceDedicated, Recreate: true, AdminUser: "a", AdminPassword: "p", MemoryMB: 512, CPUs: 1}
}

// TestRecreatePutsTheOldContainerBack: a minor upgrade whose new container
// can't be created or doesn't start leaves the old one running, on its
// release, and reports what happened.
func TestRecreatePutsTheOldContainerBack(t *testing.T) {
	for _, mode := range []string{"create", "start"} {
		t.Run(mode, func(t *testing.T) {
			in, f := newFakeInstances(t, mode)
			_, err := in.Create(context.Background(), recreateSpec())
			if err == nil || !strings.Contains(err.Error(), "the old container was started again") {
				t.Fatalf("recreate: %v", err)
			}
			name, _, _ := names(testInstance)
			c := f.containers[name]
			if c == nil || c.ID != "old" || !c.Running {
				t.Fatalf("instance container after a failed recreate: %+v", c)
			}
			if _, ok := f.containers[previousName(testInstance)]; ok {
				t.Fatal("the set-aside container is still there")
			}
			got, err := in.info(context.Background(), testInstance)
			if err != nil || got.Version != "18.1" || !got.Running {
				t.Fatalf("info: %+v %v", got, err)
			}
		})
	}
}

// TestRecreateReplacesTheContainer: on success the old container goes.
func TestRecreateReplacesTheContainer(t *testing.T) {
	in, f := newFakeInstances(t, "")
	res, err := in.Create(context.Background(), recreateSpec())
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "18.2" || len(f.containers) != 1 {
		t.Fatalf("after recreate: %+v, %d containers", res, len(f.containers))
	}
}

// TestCreatePutsBackAContainerARecreateLeft: an agent that stopped with
// the old container set aside and no new one finds it on the next create.
func TestCreatePutsBackAContainerARecreateLeft(t *testing.T) {
	in, f := newFakeInstances(t, "")
	name, _, _ := names(testInstance)
	c := f.containers[name]
	delete(f.containers, name)
	c.Name, c.Running, c.Status = previousName(testInstance), false, "exited"
	f.containers[c.Name] = c
	spec := recreateSpec()
	spec.Recreate = false
	if _, err := in.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := f.containers[name]; got == nil || got.ID != "old" || !got.Running || len(f.containers) != 1 {
		t.Fatalf("containers: %+v", f.containers)
	}
}

// TestInfoReportsNoReleaseWhenStopped: a container that isn't running runs
// no release.
func TestInfoReportsNoReleaseWhenStopped(t *testing.T) {
	in, f := newFakeInstances(t, "")
	name, _, _ := names(testInstance)
	f.containers[name].Running, f.containers[name].Status = false, "restarting"
	got, err := in.info(context.Background(), testInstance)
	if err != nil || got.Version != "" || got.ImageVersion != "18.2" {
		t.Fatalf("info: %+v %v", got, err)
	}
}
