package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/provision"
)

// The instance image is built from a pinned minor release of each
// supported major, not the moving postgres:<major> tag, and tagged with
// that release as well as the major the agent's template names.
func TestInstanceImagePinsTheRelease(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/images/postgres/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	df := string(raw)
	for _, major := range provision.DefaultPGVersions {
		m := strconv.Itoa(major)
		arg := regexp.MustCompile(`(?m)^ARG PG` + m + `_RELEASE=(\d+)\.(\d+)$`).FindStringSubmatch(df)
		if arg == nil || arg[1] != m {
			t.Fatalf("Postgres %d: no pinned release (ARG PG%s_RELEASE=%s.<minor>): %v", major, m, m, arg)
		}
		if !strings.Contains(df, "FROM ${REGISTRY}postgres:${PG"+m+"_RELEASE}-trixie AS pg"+m+"\n") {
			t.Fatalf("Postgres %d: no stage built from the pinned release", major)
		}
	}
	if regexp.MustCompile(`(?m)^FROM \$\{REGISTRY\}postgres:\$\{PG_MAJOR\}`).MatchString(df) {
		t.Fatal("the image is still built from the moving postgres:<major> tag")
	}
	if !strings.Contains(df, "FROM pg${PG_MAJOR}\n") {
		t.Fatal("the final stage doesn't pick the major's pinned stage")
	}
	build, err := os.ReadFile("../../deploy/images/postgres/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(build), `docker tag "$tag" "xenibase-postgres:$release-$walg"`) {
		t.Fatal("build.sh doesn't tag the release")
	}
	if !strings.Contains(DefaultPGImage, "{major}") {
		t.Fatalf("DefaultPGImage %q lost its {major} template", DefaultPGImage)
	}
}
