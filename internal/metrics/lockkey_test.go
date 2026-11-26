package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAdvisoryLockKeysDistinct: the background loops that run on one
// server at a time each hold a session advisory lock keyed "xenibase\xNN".
// Two loops sharing a key would leave one of them never running wherever
// the other holds it (the collector once shared the job scheduler's key and
// collected nothing in a full server), so every key must appear once.
func TestAdvisoryLockKeysDistinct(t *testing.T) {
	key := regexp.MustCompile(`0x7067646f636b[0-9a-fA-F]{2}\b`)
	seen := map[string]string{}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, k := range key.FindAllString(string(src), -1) {
				k = strings.ToLower(k)
				if prev, ok := seen[k]; ok && prev != path {
					t.Errorf("advisory lock key %s is used by both %s and %s", k, prev, path)
				}
				seen[k] = path
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("found %d lock keys; the scan is not reading the source", len(seen))
	}
}
