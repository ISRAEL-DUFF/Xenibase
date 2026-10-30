// Command testsummary reads `go test -json` on standard input and prints
// what a person needs from it: each package's result as it finishes, the
// full output of every failing test, and at the end the counts and every
// skipped test with its reason, grouped. A skip is otherwise silent in a
// run without -v, so a test that never ran looks like one that passed.
//
//	go test -json ./... | go run ./internal/tools/testsummary
//
// It exits 1 when anything failed (a test, a package or a build), or when a
// test named in XENIBASE_TEST_MUST_RUN (comma-separated test names) was
// skipped or never ran: CI sets it for the tests its environment exists
// for. Run it under `set -o pipefail` so go test's own exit status counts.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

func main() {
	var must []string
	for _, n := range strings.Split(os.Getenv("XENIBASE_TEST_MUST_RUN"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			must = append(must, n)
		}
	}
	s := newSummary(os.Stdout)
	if err := s.read(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "testsummary:", err)
		os.Exit(1)
	}
	if !s.report(must) {
		os.Exit(1)
	}
}

// event is one line of `go test -json` (cmd/test2json).
type event struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string // build-output and build-fail
}

type key struct{ pkg, test string }

type summary struct {
	w io.Writer
	// Output so far of each test that hasn't finished.
	running map[key][]string
	order   []key // running tests, in start order
	// Package-level output (no test), printed when a package finishes.
	pkgOut map[string][]string

	passed, failed, skipped      int
	pkgOK, pkgFailed, pkgNoTests int
	failures                     []key
	skips                        []skip
	ran                          map[string]bool // test names that passed or failed
	buildFailed                  []string
}

type skip struct {
	key
	reason string
}

func newSummary(w io.Writer) *summary {
	return &summary{w: w, running: map[key][]string{}, pkgOut: map[string][]string{}, ran: map[string]bool{}}
}

func (s *summary) read(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e event
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &e) != nil {
			// Not JSON (a vet or linker message go test printed itself).
			fmt.Fprintln(s.w, string(line))
			continue
		}
		s.handle(e)
	}
	return sc.Err()
}

func (s *summary) handle(e event) {
	k := key{e.Package, e.Test}
	switch e.Action {
	case "build-output":
		fmt.Fprint(s.w, e.Output)
	case "build-fail":
		s.buildFailed = append(s.buildFailed, e.ImportPath)
	case "run":
		if e.Test != "" {
			if _, ok := s.running[k]; !ok {
				s.order = append(s.order, k)
			}
			s.running[k] = nil
		}
	case "output":
		if e.Test == "" {
			s.pkgOut[e.Package] = append(s.pkgOut[e.Package], e.Output)
		} else {
			s.running[k] = append(s.running[k], e.Output)
		}
	case "pass", "fail", "skip":
		if e.Test == "" {
			s.finishPackage(e)
			return
		}
		out := s.running[k]
		delete(s.running, k)
		switch e.Action {
		case "pass":
			s.passed++
			s.ran[e.Test] = true
		case "fail":
			s.failed++
			s.ran[e.Test] = true
			s.failures = append(s.failures, k)
			fmt.Fprint(s.w, strings.Join(out, ""))
		case "skip":
			s.skipped++
			s.skips = append(s.skips, skip{k, skipReason(out)})
		}
	}
}

func (s *summary) finishPackage(e event) {
	// A test still running when its package ended (a panic, a timeout):
	// its output is all there is to go on.
	for _, k := range s.order {
		if out, ok := s.running[k]; ok && k.pkg == e.Package {
			fmt.Fprint(s.w, strings.Join(out, ""))
			delete(s.running, k)
		}
	}
	out := s.pkgOut[e.Package]
	delete(s.pkgOut, e.Package)
	switch e.Action {
	case "pass":
		s.pkgOK++
		// The last line is "ok  pkg 1.2s"; earlier ones are PASS and the like.
		s.printLast(out, "ok  \t"+e.Package)
	case "skip":
		s.pkgNoTests++
	case "fail":
		s.pkgFailed++
		for _, l := range out {
			if l != "PASS\n" {
				fmt.Fprint(s.w, l)
			}
		}
	}
}

func (s *summary) printLast(out []string, fallback string) {
	for i := len(out) - 1; i >= 0; i-- {
		if strings.HasPrefix(out[i], "ok ") {
			fmt.Fprint(s.w, out[i])
			return
		}
	}
	fmt.Fprintln(s.w, fallback)
}

// fileLine is the "name_test.go:12: " t.Log puts before a message.
var fileLine = regexp.MustCompile(`^[\w.-]+\.go:\d+: `)

// skipReason is what the test logged before skipping, on one line.
func skipReason(out []string) string {
	var parts []string
	for _, l := range out {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- SKIP") {
			continue
		}
		parts = append(parts, fileLine.ReplaceAllString(t, ""))
	}
	r := strings.Join(parts, " ")
	if len(r) > 200 {
		r = r[:200] + "…"
	}
	if r == "" {
		r = "(no reason given)"
	}
	return r
}

// report prints the totals, the skips and the failures, and reports whether
// the run passed.
func (s *summary) report(must []string) bool {
	ok := s.failed == 0 && s.pkgFailed == 0 && len(s.buildFailed) == 0
	fmt.Fprintf(s.w, "\nTests: %d passed, %d failed, %d skipped. Packages: %d ok, %d failed, %d without tests.\n",
		s.passed, s.failed, s.skipped, s.pkgOK, s.pkgFailed, s.pkgNoTests)

	if len(s.skips) > 0 {
		type group struct {
			pkg, reason string
			tests       []string
		}
		byKey := map[[2]string]*group{}
		var groups []*group
		for _, sk := range s.skips {
			gk := [2]string{sk.pkg, sk.reason}
			g, found := byKey[gk]
			if !found {
				g = &group{pkg: sk.pkg, reason: sk.reason}
				byKey[gk] = g
				groups = append(groups, g)
			}
			g.tests = append(g.tests, sk.test)
		}
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].pkg < groups[j].pkg })
		fmt.Fprintf(s.w, "\nSkipped (%d):\n", len(s.skips))
		for _, g := range groups {
			names := g.tests
			more := ""
			if len(names) > 8 {
				more = fmt.Sprintf(" and %d more", len(names)-8)
				names = names[:8]
			}
			fmt.Fprintf(s.w, "  %s: %s\n    %s%s\n", g.pkg, g.reason, strings.Join(names, ", "), more)
		}
	}

	var missing []string
	for _, n := range must {
		if !s.ran[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		ok = false
		fmt.Fprintf(s.w, "\nMust run (XENIBASE_TEST_MUST_RUN) but skipped or not run: %s\n", strings.Join(missing, ", "))
	}
	if len(s.buildFailed) > 0 {
		fmt.Fprintf(s.w, "\nBuild failed: %s\n", strings.Join(s.buildFailed, ", "))
	}
	if len(s.failures) > 0 {
		fmt.Fprintf(s.w, "\nFailed (%d):\n", len(s.failures))
		for _, f := range s.failures {
			fmt.Fprintf(s.w, "  %s %s\n", f.pkg, f.test)
		}
	}
	if s.pkgFailed > 0 && len(s.failures) == 0 {
		fmt.Fprintln(s.w, "\nA package failed without a failing test (see its output above).")
	}
	return ok
}
