package dedicated

import (
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/provision"
)

func TestReplicaSettings(t *testing.T) {
	if got := DefaultReplicaSettings.parameters(); got["hot_standby_feedback"] != "off" ||
		got["max_standby_streaming_delay"] != "30s" || got["max_standby_archive_delay"] != "30s" {
		t.Fatalf("defaults: %v", got)
	}
	on := ReplicaSettings{HotStandbyFeedback: true, MaxStandbyDelay: 120}.parameters()
	if on["hot_standby_feedback"] != "on" || on["max_standby_streaming_delay"] != "120s" {
		t.Fatalf("on, 120 s: %v", on)
	}
	for _, d := range []int{0, 30, MaxStandbyDelay} {
		if err := (ReplicaSettings{MaxStandbyDelay: d}).Validate(); err != nil {
			t.Errorf("%d s refused: %v", d, err)
		}
	}
	for _, d := range []int{-1, MaxStandbyDelay + 1} {
		if err := (ReplicaSettings{MaxStandbyDelay: d}).Validate(); !errors.Is(err, provision.ErrInvalid) {
			t.Errorf("%d s: %v", d, err)
		}
	}
	for _, v := range []string{"on", "off", "0s", "900s"} {
		if !settingValue.MatchString(v) {
			t.Errorf("%q not accepted", v)
		}
	}
	for _, v := range []string{"on'; DROP", "30", "-1s", ""} {
		if settingValue.MatchString(v) {
			t.Errorf("%q accepted", v)
		}
	}
}
