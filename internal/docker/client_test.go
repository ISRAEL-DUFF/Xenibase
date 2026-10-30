package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAgainstDaemon runs when XENIBASE_TEST_DOCKER=1, and then fails without a
// daemon or the image (postgres:18, or XENIBASE_TEST_DOCKER_IMAGE).
func TestAgainstDaemon(t *testing.T) {
	if os.Getenv("XENIBASE_TEST_DOCKER") == "" {
		t.Skip("XENIBASE_TEST_DOCKER not set")
	}
	image := os.Getenv("XENIBASE_TEST_DOCKER_IMAGE")
	if image == "" {
		image = "postgres:18"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := New(os.Getenv("DOCKER_HOST"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("XENIBASE_TEST_DOCKER is set but there is no docker daemon: %v", err)
	}
	if ok, err := c.ImageExists(ctx, image); err != nil || !ok {
		t.Fatalf("image %s not present (docker pull it, or set XENIBASE_TEST_DOCKER_IMAGE): %v", image, err)
	}
	if ok, _ := c.ImageExists(ctx, "xenibase/definitely-missing:0"); ok {
		t.Fatal("missing image reported present")
	}
	name := "xenibase-docker-test"
	_ = c.RemoveContainer(ctx, name)
	if err := c.CreateVolume(ctx, name, map[string]string{"xenibase.test": "1"}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = c.RemoveContainer(context.Background(), name)
		_ = c.RemoveVolume(context.Background(), name)
	}()
	id, err := c.CreateContainer(ctx, name, ContainerConfig{
		Image: image, Entrypoint: []string{"sh", "-c"}, Cmd: []string{"echo started; sleep 60"},
		HostConfig: HostConfig{Mounts: []Mount{{Type: "volume", Source: name, Target: "/data"}}, Memory: 64 << 20, NanoCPUs: 500_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	r, err := c.Exec(ctx, id, "", []string{"X=42"}, []string{"sh", "-c", "echo out $X; echo err >&2; exit 3"})
	if err != nil || r.ExitCode != 3 || strings.TrimSpace(r.Stdout) != "out 42" || strings.TrimSpace(r.Stderr) != "err" {
		t.Fatalf("exec: %+v %v", r, err)
	}
	info, err := c.InspectContainer(ctx, name)
	if err != nil || !info.State.Running {
		t.Fatalf("inspect: %+v %v", info.State, err)
	}
	logs, _ := c.Logs(ctx, id, 10)
	if !strings.Contains(logs, "started") {
		t.Fatalf("logs: %q", logs)
	}
	if err := c.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InspectContainer(ctx, id); err == nil {
		t.Fatal("container still there")
	}
}
