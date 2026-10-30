package statusapi

import (
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/signature"
)

// SignPush's v2 binds the method and path; its v1 still verifies with the
// body-only scheme older status services check, so xenibase-server can be
// upgraded first.
func TestSignPush(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	body := []byte(`{"components":[]}`)
	h := SignPush("s3cret", now, "POST", PathHeartbeat, body)
	if err := VerifyPush("s3cret", h, "POST", PathHeartbeat, body, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{
		{"PUT", PathManagedSubscribers}, {"PUT", PathSLATargets}, {"PUT", PathHeartbeat}, {"POST", PathSLAResults},
	} {
		if VerifyPush("s3cret", h, c.method, c.path, body, now, 5*time.Minute) == nil {
			t.Errorf("a heartbeat's signature verified for %s %s", c.method, c.path)
		}
	}
	if VerifyPush("other", h, "POST", PathHeartbeat, body, now, 5*time.Minute) == nil {
		t.Error("another secret verified")
	}
	if VerifyPush("s3cret", h, "POST", PathHeartbeat, body, now.Add(6*time.Minute), 5*time.Minute) == nil {
		t.Error("an old timestamp verified")
	}
	if VerifyPush("s3cret", signature.Sign("s3cret", now, body), "POST", PathHeartbeat, body, now, 5*time.Minute) == nil {
		t.Error("a v1-only header verified")
	}
	if err := signature.Verify("s3cret", h, body, now, 5*time.Minute); err != nil {
		t.Errorf("an older status service's check: %v", err)
	}
}
