package schedjobs

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestParseAndNext(t *testing.T) {
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		expr, tz, from, want string
	}{
		{"*/5 * * * *", "UTC", "2026-10-03 12:03", "2026-10-03 12:05"},
		{"0 3 * * *", "UTC", "2026-10-03 12:03", "2026-10-04 03:00"},
		{"30 9 * * mon-fri", "UTC", "2026-10-03 12:00", "2026-10-05 09:30"}, // Saturday → Monday
		{"0 0 1 * *", "UTC", "2026-10-03 00:00", "2026-11-01 00:00"},
		{"@hourly", "UTC", "2026-10-03 12:00", "2026-10-03 13:00"},
		{"0 9 * * *", "Europe/Berlin", "2026-10-03 06:00", "2026-10-03 07:00"}, // 09:00 CEST
		{"0 0 13 * 5", "UTC", "2026-10-03 00:00", "2026-10-09 00:00"},          // Friday or the 13th
		{"5/20 * * * *", "UTC", "2026-10-03 12:06", "2026-10-03 12:25"},
		{"0 12 * * 7", "UTC", "2026-10-03 13:00", "2026-10-04 12:00"}, // 7 = Sunday
	} {
		s, err := Parse(c.expr, c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(utc(c.from)).UTC(); !got.Equal(utc(c.want)) {
			t.Errorf("%s in %s after %s: %s, want %s", c.expr, c.tz, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "a b c d e", "5-1 * * * *"} {
		if _, err := Parse(bad, "UTC"); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := Parse("* * * * *", "Mars/Olympus"); err == nil {
		t.Error("an unknown time zone parsed")
	}
	if s, _ := Parse("0 0 30 2 *", "UTC"); !s.Next(utc("2026-01-01 00:00")).IsZero() {
		t.Error("30 February has a next run")
	}
}

func TestMinGap(t *testing.T) {
	from := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	for expr, want := range map[string]time.Duration{
		"* * * * *": time.Minute, "*/5 * * * *": 5 * time.Minute, "0 * * * *": time.Hour,
		"0,1 * * * *": time.Minute, "0 3 * * *": 24 * time.Hour,
	} {
		s, err := Parse(expr, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		if got := s.MinGap(from); got != want {
			t.Errorf("%s: %s, want %s", expr, got, want)
		}
	}
}

// TestNextAcrossDST walks schedules through the 2026 transitions in New
// York (01:00–02:00 repeats on 1 November, 02:00–03:00 is skipped on 8
// March) and London (01:00–02:00 repeats on 25 October, is skipped on 29
// March). Times are UTC.
func TestNextAcrossDST(t *testing.T) {
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			v, err = time.Parse("2006-01-02 15:04", s)
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const ny, lon = "America/New_York", "Europe/London"
	for _, c := range []struct {
		name, expr, tz, from string
		want                 []string
	}{
		// New York, fall-back: a fixed time fires at its first occurrence only.
		{"ny fall daily", "30 1 * * *", ny, "2026-10-31 12:00", []string{"2026-11-01 05:30", "2026-11-02 06:30"}},
		{"ny fall daily top", "0 1 * * *", ny, "2026-10-31 12:00", []string{"2026-11-01 05:00", "2026-11-02 06:00"}},
		{"ny fall two minutes", "0,30 1 * * *", ny, "2026-11-01 04:00", []string{"2026-11-01 05:00", "2026-11-01 05:30", "2026-11-02 06:00"}},
		{"ny fall after repeat", "0 2 * * *", ny, "2026-10-31 12:00", []string{"2026-11-01 07:00", "2026-11-02 07:00"}},
		{"ny fall from first fire", "30 1 * * *", ny, "2026-11-01 05:30:04", []string{"2026-11-02 06:30"}},
		{"ny fall from inside repeat", "30 1 * * *", ny, "2026-11-01 06:10", []string{"2026-11-02 06:30"}},
		// ... while * in the minute or hour runs by the clock through both.
		{"ny fall hourly", "30 * * * *", ny, "2026-11-01 04:00", []string{"2026-11-01 04:30", "2026-11-01 05:30", "2026-11-01 06:30", "2026-11-01 07:30"}},
		{"ny fall every 20", "*/20 * * * *", ny, "2026-11-01 05:30", []string{"2026-11-01 05:40", "2026-11-01 06:00", "2026-11-01 06:20", "2026-11-01 06:40"}},
		{"ny fall every minute", "* * * * *", ny, "2026-11-01 05:58", []string{"2026-11-01 05:59", "2026-11-01 06:00", "2026-11-01 06:01"}},
		{"ny fall every 30 in hour 1", "*/30 1 * * *", ny, "2026-11-01 04:00", []string{"2026-11-01 05:00", "2026-11-01 05:30", "2026-11-01 06:00", "2026-11-01 06:30", "2026-11-02 06:00"}},
		// New York, spring-forward: a fixed time in the gap fires at 03:00 EDT.
		{"ny spring daily", "30 2 * * *", ny, "2026-03-07 12:00", []string{"2026-03-08 07:00", "2026-03-09 06:30"}},
		{"ny spring daily top", "0 2 * * *", ny, "2026-03-07 12:00", []string{"2026-03-08 07:00", "2026-03-09 06:00"}},
		{"ny spring after gap", "0 3 * * *", ny, "2026-03-07 12:00", []string{"2026-03-08 07:00", "2026-03-09 07:00"}},
		{"ny spring gap and after", "30 2,3 * * *", ny, "2026-03-08 05:00", []string{"2026-03-08 07:00", "2026-03-08 07:30", "2026-03-09 06:30"}},
		{"ny spring gap and 03:00", "0 2,3 * * *", ny, "2026-03-08 05:00", []string{"2026-03-08 07:00", "2026-03-09 06:00", "2026-03-09 07:00"}},
		{"ny spring sunday", "30 2 * * 0", ny, "2026-03-07 12:00", []string{"2026-03-08 07:00", "2026-03-15 06:30"}},
		{"ny spring monday", "30 2 * * 1", ny, "2026-03-07 12:00", []string{"2026-03-09 06:30"}},
		{"ny spring from just before", "30 2 * * *", ny, "2026-03-08 06:59:30", []string{"2026-03-08 07:00"}},
		{"ny spring from gap fire", "30 2 * * *", ny, "2026-03-08 07:00:05", []string{"2026-03-09 06:30"}},
		{"ny spring hourly", "30 * * * *", ny, "2026-03-08 06:00", []string{"2026-03-08 06:30", "2026-03-08 07:30", "2026-03-08 08:30"}},
		{"ny spring every 15", "*/15 * * * *", ny, "2026-03-08 06:40", []string{"2026-03-08 06:45", "2026-03-08 07:00", "2026-03-08 07:15"}},
		{"ny spring every 15 in hour 2", "*/15 2 * * *", ny, "2026-03-08 05:00", []string{"2026-03-09 06:00"}},
		// London, fall-back.
		{"lon fall daily", "30 1 * * *", lon, "2026-10-24 12:00", []string{"2026-10-25 00:30", "2026-10-26 01:30"}},
		{"lon fall after repeat", "0 2 * * *", lon, "2026-10-24 12:00", []string{"2026-10-25 02:00", "2026-10-26 02:00"}},
		{"lon fall from first fire", "30 1 * * *", lon, "2026-10-25 00:30:03", []string{"2026-10-26 01:30"}},
		{"lon fall hourly", "30 * * * *", lon, "2026-10-24 23:00", []string{"2026-10-24 23:30", "2026-10-25 00:30", "2026-10-25 01:30", "2026-10-25 02:30"}},
		{"lon fall every 30 in hour 1", "*/30 1 * * *", lon, "2026-10-24 23:00", []string{"2026-10-25 00:00", "2026-10-25 00:30", "2026-10-25 01:00", "2026-10-25 01:30", "2026-10-26 01:00"}},
		{"lon fall weekly", "0 1 * * sun", lon, "2026-10-24 12:00", []string{"2026-10-25 00:00", "2026-11-01 01:00"}},
		// London, spring-forward: a fixed time in the gap fires at 02:00 BST.
		{"lon spring daily", "30 1 * * *", lon, "2026-03-28 12:00", []string{"2026-03-29 01:00", "2026-03-30 00:30"}},
		{"lon spring daily top", "0 1 * * *", lon, "2026-03-28 12:00", []string{"2026-03-29 01:00", "2026-03-30 00:00"}},
		{"lon spring every 10", "*/10 * * * *", lon, "2026-03-29 00:45", []string{"2026-03-29 00:50", "2026-03-29 01:00", "2026-03-29 01:10"}},
		{"lon spring hourly", "30 * * * *", lon, "2026-03-29 00:00", []string{"2026-03-29 00:30", "2026-03-29 01:30", "2026-03-29 02:30"}},
		{"lon spring every 10 in hour 1", "*/10 1 * * *", lon, "2026-03-29 00:00", []string{"2026-03-30 00:00"}},
		{"lon spring monthly", "30 1 29 * *", lon, "2026-03-01 00:00", []string{"2026-03-29 01:00", "2026-04-29 00:30"}},
	} {
		s, err := Parse(c.expr, c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, r := range s.Upcoming(utc(c.from), len(c.want)) {
			got = append(got, r.UTC().Format("2006-01-02 15:04"))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %s in %s after %s:\n got %v\nwant %v", c.name, c.expr, c.tz, c.from, got, c.want)
		}
	}
}

// TestNextDSTYear checks a year of runs in zones with daylight saving
// (Lord Howe's shift is half an hour, Santiago's is at midnight): every run
// is at least a minute after the one before, a fixed daily time fires on
// every local date exactly once, and an every-N-minutes schedule never
// waits longer than N minutes of real time.
func TestNextDSTYear(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tz := range []string{"America/New_York", "Europe/London", "Australia/Lord_Howe", "America/Santiago", "UTC"} {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			t.Fatal(err)
		}
		for _, expr := range []string{"30 1 * * *", "0 2 * * *", "30 2 * * *", "45 0 * * *", "0 0 * * *", "15 23 * * *"} {
			s, err := Parse(expr, tz)
			if err != nil {
				t.Fatal(err)
			}
			days := map[string]bool{}
			prev := from
			for _, r := range s.Upcoming(from, 365) {
				if r.Sub(prev) < time.Minute {
					t.Fatalf("%s in %s: %s only %s after %s", expr, tz, r, r.Sub(prev), prev)
				}
				days[r.In(loc).Format("2006-01-02")] = true
				prev = r
			}
			if len(days) != 365 {
				t.Errorf("%s in %s: 365 runs fall on %d dates", expr, tz, len(days))
			}
		}
		for _, n := range []int{1, 5, 15, 30} {
			s, err := Parse("*/"+strconv.Itoa(n)+" * * * *", tz)
			if err != nil {
				t.Fatal(err)
			}
			prev := s.Next(from)
			for range 366 * 24 * 60 / n {
				next := s.Next(prev)
				if d := next.Sub(prev); d < time.Minute || d > time.Duration(n)*time.Minute {
					t.Fatalf("*/%d in %s: %s after %s (%s)", n, tz, next, prev, d)
				}
				prev = next
			}
		}
	}
}
