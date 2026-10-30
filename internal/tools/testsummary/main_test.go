package main

import (
	"bytes"
	"strings"
	"testing"
)

const run = `{"Action":"start","Package":"x/a"}
{"Action":"run","Package":"x/a","Test":"TestOK"}
{"Action":"output","Package":"x/a","Test":"TestOK","Output":"=== RUN   TestOK\n"}
{"Action":"output","Package":"x/a","Test":"TestOK","Output":"    a_test.go:9: quiet when it passes\n"}
{"Action":"pass","Package":"x/a","Test":"TestOK"}
{"Action":"run","Package":"x/a","Test":"TestNeedsPG"}
{"Action":"output","Package":"x/a","Test":"TestNeedsPG","Output":"=== RUN   TestNeedsPG\n"}
{"Action":"output","Package":"x/a","Test":"TestNeedsPG","Output":"    a_test.go:12: XENIBASE_TEST_PG_BIN not set\n"}
{"Action":"output","Package":"x/a","Test":"TestNeedsPG","Output":"--- SKIP: TestNeedsPG (0.00s)\n"}
{"Action":"skip","Package":"x/a","Test":"TestNeedsPG"}
{"Action":"output","Package":"x/a","Output":"PASS\n"}
{"Action":"output","Package":"x/a","Output":"ok  \tx/a\t0.01s\n"}
{"Action":"pass","Package":"x/a"}
{"Action":"run","Package":"x/b","Test":"TestBroken"}
{"Action":"output","Package":"x/b","Test":"TestBroken","Output":"=== RUN   TestBroken\n"}
{"Action":"output","Package":"x/b","Test":"TestBroken","Output":"    b_test.go:5: got 1, want 2\n"}
{"Action":"output","Package":"x/b","Test":"TestBroken","Output":"--- FAIL: TestBroken (0.00s)\n"}
{"Action":"fail","Package":"x/b","Test":"TestBroken"}
{"Action":"output","Package":"x/b","Output":"FAIL\n"}
{"Action":"output","Package":"x/b","Output":"FAIL\tx/b\t0.01s\n"}
{"Action":"fail","Package":"x/b"}
{"Action":"output","Package":"x/c","Output":"?   \tx/c\t[no test files]\n"}
{"Action":"skip","Package":"x/c"}
`

func TestSummary(t *testing.T) {
	var out bytes.Buffer
	s := newSummary(&out)
	if err := s.read(strings.NewReader(run)); err != nil {
		t.Fatal(err)
	}
	if s.report(nil) {
		t.Fatal("a run with a failure passed")
	}
	got := out.String()
	for _, want := range []string{
		"ok  \tx/a\t0.01s\n",
		"b_test.go:5: got 1, want 2", "--- FAIL: TestBroken", "FAIL\tx/b",
		"Tests: 1 passed, 1 failed, 1 skipped. Packages: 1 ok, 1 failed, 1 without tests.",
		"Skipped (1):\n  x/a: XENIBASE_TEST_PG_BIN not set\n    TestNeedsPG\n",
		"Failed (1):\n  x/b TestBroken\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "quiet when it passes") {
		t.Errorf("a passing test's output was printed:\n%s", got)
	}
}

func TestMustRun(t *testing.T) {
	passing := strings.SplitAfter(run, `{"Action":"run","Package":"x/b"`)[0]
	passing = strings.TrimSuffix(passing, `{"Action":"run","Package":"x/b"`)
	for must, want := range map[string]bool{"": true, "TestOK": true, "TestNeedsPG": false, "TestMissing": false} {
		var out bytes.Buffer
		s := newSummary(&out)
		if err := s.read(strings.NewReader(passing)); err != nil {
			t.Fatal(err)
		}
		var m []string
		if must != "" {
			m = []string{must}
		}
		if got := s.report(m); got != want {
			t.Errorf("must run %q: %v, want %v\n%s", must, got, want, out.String())
		}
	}
}

func TestBuildFailureAndStrayLines(t *testing.T) {
	var out bytes.Buffer
	s := newSummary(&out)
	in := `# x/d
{"ImportPath":"x/d","Action":"build-output","Output":"d.go:3:1: syntax error\n"}
{"ImportPath":"x/d","Action":"build-fail"}
{"Action":"output","Package":"x/d","Output":"FAIL\tx/d [build failed]\n"}
{"Action":"fail","Package":"x/d"}
`
	if err := s.read(strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if s.report(nil) {
		t.Fatal("a build failure passed")
	}
	for _, want := range []string{"# x/d\n", "d.go:3:1: syntax error", "FAIL\tx/d [build failed]", "Build failed: x/d"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
