package provision

import "testing"

func TestMaxConnectionsFor(t *testing.T) {
	for _, c := range []struct{ memMB, want int }{
		{512, 100}, {1024, 100}, {2048, 100}, {3072, 150}, {4096, 200}, {8192, 400}, {12288, 500}, {32768, 500},
	} {
		if got := MaxConnectionsFor(c.memMB); got != c.want {
			t.Errorf("MaxConnectionsFor(%d) = %d, want %d", c.memMB, got, c.want)
		}
	}
	if got := ConnectionLimitMax(100); got != 90 {
		t.Errorf("ConnectionLimitMax(100) = %d, want 90 (the dedicated default)", got)
	}
	if got := ConnectionLimitMax(400); got != 390 {
		t.Errorf("ConnectionLimitMax(400) = %d", got)
	}
}
