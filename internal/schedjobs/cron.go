package schedjobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression (minute hour day-of-month
// month day-of-week) in a time zone.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
	// fixed is a schedule whose minute and hour are both given (neither
	// starts with *): it fires once on the day a wall-clock hour repeats,
	// and at the end of a skipped hour it fell in (as cronie does).
	fixed bool
	loc   *time.Location
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse reads a cron expression in time zone tz (an IANA name).
func Parse(expr, tz string) (Schedule, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		return Schedule{}, fmt.Errorf("unknown time zone %q", tz)
	}
	expr = strings.TrimSpace(strings.ToLower(expr))
	if m, ok := macros[expr]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("a cron expression has 5 fields (minute hour day month weekday), not %d", len(f))
	}
	star := func(f string) bool { return strings.HasPrefix(f, "*") || strings.HasPrefix(f, "?") }
	s := Schedule{loc: loc, domStar: f[2] == "*" || f[2] == "?", dowStar: f[4] == "*" || f[4] == "?", fixed: !star(f[0]) && !star(f[1])}
	specs := []struct {
		dst      *uint64
		min, max int
		names    map[string]int
		what     string
	}{
		{&s.minute, 0, 59, nil, "minute"}, {&s.hour, 0, 23, nil, "hour"}, {&s.dom, 1, 31, nil, "day of month"},
		{&s.month, 1, 12, monthNames, "month"}, {&s.dow, 0, 7, dayNames, "day of week"},
	}
	for i, sp := range specs {
		bits, err := field(f[i], sp.min, sp.max, sp.names)
		if err != nil {
			return Schedule{}, fmt.Errorf("%s: %w", sp.what, err)
		}
		*sp.dst = bits
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	return s, nil
}

func field(f string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(f, ",") {
		step := 1
		if r, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q", st)
			}
			part, step = r, n
		}
		a, b := lo, hi
		switch {
		case part == "*" || part == "?":
		case strings.Contains(part, "-"):
			x, y, _ := strings.Cut(part, "-")
			var err error
			if a, err = value(x, names); err != nil {
				return 0, err
			}
			if b, err = value(y, names); err != nil {
				return 0, err
			}
		default:
			v, err := value(part, names)
			if err != nil {
				return 0, err
			}
			a, b = v, v
			if step > 1 { // "5/15" means from 5 to the end
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[s]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return v, nil
}

func (s Schedule) matchDay(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default: // both restricted: either matches (cron semantics)
		return dom || dow
	}
}

// Next is the first run strictly after t (at least a minute after it), or
// the zero time if there is none within five years (e.g. 30 February).
//
// The schedule matches wall-clock time in its zone, walked one zone period
// (the span between two offset changes) at a time, so a daylight-saving
// change neither repeats nor drops a fixed-time run. On the day an hour
// repeats, a fixed-time schedule matches only its first occurrence, and a
// fixed time in an hour that is skipped fires at the first minute after
// the gap; schedules with * in the minute or hour run by the clock through
// both (Vixie cron and cronie behave the same).
func (s Schedule) Next(t time.Time) time.Time {
	cur := t.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	end := cur.AddDate(5, 0, 0)
	for cur.Before(end) {
		start, until := cur.ZoneBounds()
		_, off := cur.Zone()
		prevOff := off
		if !start.IsZero() {
			_, prevOff = start.Add(-time.Nanosecond).Zone()
		}
		if s.fixed && cur.Equal(start) && prevOff < off && s.matchesIn(wall(start, prevOff), wall(start, off)) {
			return cur // a fixed time the clocks skipped over
		}
		from := wall(cur, off)
		if s.fixed && prevOff > off {
			// The first prevOff-off of this period repeat wall-clock time
			// the previous period already showed.
			from = maxTime(from, wall(start, prevOff))
		}
		stop := wall(end, off)
		if !until.IsZero() && until.Before(end) {
			stop = wall(until, off)
		}
		if w, ok := s.nextWall(from, stop); ok {
			return time.Unix(w.Unix()-int64(off), 0).In(s.loc)
		}
		if until.IsZero() {
			break
		}
		cur = until
	}
	return time.Time{}
}

// wall is t's wall clock at offset off (seconds east of UTC) as a UTC time,
// in which there is no daylight saving to step around.
func wall(t time.Time, off int) time.Time {
	return t.UTC().Add(time.Duration(off) * time.Second)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// matchesIn is whether a wall-clock minute in [from, stop) matches.
func (s Schedule) matchesIn(from, stop time.Time) bool {
	_, ok := s.nextWall(from, stop)
	return ok
}

// nextWall is the first matching wall-clock minute at or after from and
// before stop, both wall clocks in UTC (see wall).
func (s Schedule) nextWall(from, stop time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute)
	if t.Before(from) {
		t = t.Add(time.Minute)
	}
	for t.Before(stop) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.matchDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// Upcoming lists the next n runs after t.
func (s Schedule) Upcoming(t time.Time, n int) []time.Time {
	var out []time.Time
	for range n {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// MinGap is the shortest time between two runs over the next 1,000 runs
// (bounded at a year): the interval the plan's minimum is checked against.
func (s Schedule) MinGap(from time.Time) time.Duration {
	prev := s.Next(from)
	if prev.IsZero() {
		return 0
	}
	gap := time.Duration(1<<63 - 1)
	end := prev.AddDate(1, 0, 0)
	for range 1000 {
		next := s.Next(prev)
		if next.IsZero() || next.After(end) {
			break
		}
		gap = min(gap, next.Sub(prev))
		prev = next
	}
	return gap
}
