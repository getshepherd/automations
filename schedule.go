package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is when an automation runs, in local time. Kind picks the fields that count:
//
//   - "interval": every Every minutes, counted from the last run (or from when it was saved);
//   - "daily": at At ("HH:MM") on Weekdays (0 is Sunday; none is every day);
//   - "once": at RunAt, then never again;
//   - "cron": a five-field Cron line (minute hour day-of-month month day-of-week).
type Schedule struct {
	Kind     string    `json:"kind"`
	Every    int       `json:"every,omitempty"`
	At       string    `json:"at,omitempty"`
	Weekdays []int     `json:"weekdays,omitempty"`
	RunAt    time.Time `json:"run_at,omitzero"`
	Cron     string    `json:"cron,omitempty"`
}

// Check says what is wrong with a schedule, before it is saved.
func (s Schedule) Check() error {
	switch s.Kind {
	case "interval":
		if s.Every < 1 {
			return fmt.Errorf("every: at least 1 minute")
		}
	case "daily":
		if _, _, err := clock(s.At); err != nil {
			return err
		}
		for _, d := range s.Weekdays {
			if d < 0 || d > 6 {
				return fmt.Errorf("weekday %d: 0 (Sunday) to 6", d)
			}
		}
	case "once":
		if s.RunAt.IsZero() {
			return fmt.Errorf("run_at: a time is needed")
		}
	case "cron":
		if _, err := parseCron(s.Cron); err != nil {
			return err
		}
	default:
		return fmt.Errorf("kind %q: interval, daily, once or cron", s.Kind)
	}
	return nil
}

// Next is the first run after from, the zero time for none (a once that has passed).
func (s Schedule) Next(from time.Time) time.Time {
	switch s.Kind {
	case "interval":
		return from.Add(time.Duration(max(s.Every, 1)) * time.Minute)
	case "once":
		if s.RunAt.After(from) {
			return s.RunAt
		}
	case "daily":
		h, m, err := clock(s.At)
		if err != nil {
			return time.Time{}
		}
		days := map[int]bool{}
		for _, d := range s.Weekdays {
			days[d] = true
		}
		for i := 0; i <= 7; i++ {
			// a date, then its wall clock, so a daylight-saving day keeps the hour
			d := from.AddDate(0, 0, i)
			t := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, from.Location())
			if t.After(from) && (len(days) == 0 || days[int(t.Weekday())]) {
				return t
			}
		}
	case "cron":
		c, err := parseCron(s.Cron)
		if err != nil {
			return time.Time{}
		}
		// ponytail: minute by minute over a year, at most ~527k checks; fine for a handful of automations a minute
		t := from.Truncate(time.Minute).Add(time.Minute)
		for end := from.AddDate(1, 0, 1); t.Before(end); t = t.Add(time.Minute) {
			if c.match(t) {
				return t
			}
		}
	}
	return time.Time{}
}

func clock(at string) (int, int, error) {
	h, m, ok := strings.Cut(at, ":")
	hour, err1 := strconv.Atoi(h)
	minute, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 || len(m) != 2 {
		return 0, 0, fmt.Errorf("at %q: HH:MM", at)
	}
	return hour, minute, nil
}

// cron is the five fields, each the set of values that match.
type cron struct {
	minute, hour, dom, month, dow map[int]bool
	// cron's rule: with both days restricted, either matching is enough
	domAny, dowAny bool
}

func parseCron(line string) (cron, error) {
	f := strings.Fields(line)
	if len(f) != 5 {
		return cron{}, fmt.Errorf("cron %q: five fields, minute hour day month weekday", line)
	}
	var c cron
	var err error
	ranges := []struct {
		set      *map[int]bool
		min, max int
	}{{&c.minute, 0, 59}, {&c.hour, 0, 23}, {&c.dom, 1, 31}, {&c.month, 1, 12}, {&c.dow, 0, 7}}
	for i, r := range ranges {
		if *r.set, err = cronField(f[i], r.min, r.max); err != nil {
			return cron{}, fmt.Errorf("cron %q: %w", line, err)
		}
	}
	if c.dow[7] {
		c.dow[0] = true // 7 is Sunday too
	}
	c.domAny, c.dowAny = f[2] == "*", f[4] == "*"
	return c, nil
}

// cronField reads `*`, `5`, `1-5`, `*/15`, `1-30/5` and lists of them.
func cronField(field string, lo, hi int) (map[int]bool, error) {
	set := map[int]bool{}
	for part := range strings.SplitSeq(field, ",") {
		span, step := part, 1
		if s, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%q: a step is a number from 1", part)
			}
			span, step = s, n
		}
		from, to := lo, hi
		if span != "*" {
			a, b, isRange := strings.Cut(span, "-")
			var err error
			if from, err = strconv.Atoi(a); err != nil {
				return nil, fmt.Errorf("%q: not a number", part)
			}
			to = from
			if isRange {
				if to, err = strconv.Atoi(b); err != nil {
					return nil, fmt.Errorf("%q: not a number", part)
				}
			} else if step > 1 {
				to = hi // `5/15`: from 5 on
			}
		}
		if from < lo || to > hi || from > to {
			return nil, fmt.Errorf("%q: %d to %d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return set, nil
}

func (c cron) match(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	dom, dow := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	default:
		return dom || dow
	}
}
