package main

import (
	"slices"
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Skip(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// 2026-11-06 is a Friday
	cases := []struct {
		name string
		s    Schedule
		from string
		want string // "" for none
	}{
		{"interval", Schedule{Kind: "interval", Every: 90}, "2026-11-06 10:00", "2026-11-06 11:30"},
		{"daily later today", Schedule{Kind: "daily", At: "18:30"}, "2026-11-06 10:00", "2026-11-06 18:30"},
		{"daily passed", Schedule{Kind: "daily", At: "09:00"}, "2026-11-06 10:00", "2026-11-07 09:00"},
		{"daily on the minute is the next one", Schedule{Kind: "daily", At: "10:00"}, "2026-11-06 10:00", "2026-11-07 10:00"},
		{"weekdays skip the weekend", Schedule{Kind: "daily", At: "09:00", Weekdays: []int{1, 2, 3, 4, 5}}, "2026-11-06 10:00", "2026-11-09 09:00"},
		{"daily over daylight saving keeps the hour", Schedule{Kind: "daily", At: "09:00"}, "2026-10-24 10:00", "2026-10-25 09:00"},
		{"once ahead", Schedule{Kind: "once", RunAt: at("2026-12-01 12:00")}, "2026-11-06 10:00", "2026-12-01 12:00"},
		{"once passed", Schedule{Kind: "once", RunAt: at("2026-11-01 12:00")}, "2026-11-06 10:00", ""},
		{"cron weekdays 9:00", Schedule{Kind: "cron", Cron: "0 9 * * 1-5"}, "2026-11-06 10:00", "2026-11-09 09:00"},
		{"cron every 15", Schedule{Kind: "cron", Cron: "*/15 * * * *"}, "2026-11-06 10:07", "2026-11-06 10:15"},
		{"cron sunday as 7", Schedule{Kind: "cron", Cron: "30 8 * * 7"}, "2026-11-06 10:00", "2026-11-08 08:30"},
		{"cron day or weekday", Schedule{Kind: "cron", Cron: "0 12 13 * 1"}, "2026-11-06 10:00", "2026-11-09 12:00"},
		{"cron never (Feb 30)", Schedule{Kind: "cron", Cron: "0 0 30 2 *"}, "2026-11-06 10:00", ""},
	}
	for _, c := range cases {
		got := c.s.Next(at(c.from))
		want := time.Time{}
		if c.want != "" {
			want = at(c.want)
		}
		if !got.Equal(want) {
			t.Errorf("%s: %v, want %v", c.name, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	bad := []Schedule{
		{Kind: "interval"},
		{Kind: "daily", At: "9:5"},
		{Kind: "daily", At: "24:00"},
		{Kind: "daily", At: "09:00", Weekdays: []int{7}},
		{Kind: "once"},
		{Kind: "cron", Cron: "* * * *"},
		{Kind: "cron", Cron: "60 * * * *"},
		{Kind: "cron", Cron: "*/0 * * * *"},
		{Kind: "hourly"},
	}
	for _, s := range bad {
		if s.Check() == nil {
			t.Errorf("%+v passed", s)
		}
	}
	if err := (Schedule{Kind: "cron", Cron: "5,35 9-17/2 * 1-6 1-5"}).Check(); err != nil {
		t.Error(err)
	}
}

func TestDays(t *testing.T) {
	for in, want := range map[string][]int{"mon-fri": {1, 2, 3, 4, 5}, "sat,sun": {6, 0}, "1-3,5": {1, 2, 3, 5}, "Monday": {1}} {
		s, err := saveInput{Kind: "daily", At: "09:00", Days: []string{in}}.schedule()
		if err != nil || !slices.Equal(s.Weekdays, want) {
			t.Errorf("%s: %v %v, want %v", in, s.Weekdays, err, want)
		}
	}
	for _, in := range []string{"fri-mon", "7", "xyz", "mo"} {
		if _, err := (saveInput{Kind: "daily", At: "09:00", Days: []string{in}}).schedule(); err == nil {
			t.Errorf("%s passed", in)
		}
	}
}

func TestMissed(t *testing.T) {
	due := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		grace int
		late  time.Duration
		want  bool
	}{
		{0, 10 * time.Minute, false}, // the default, 10 min
		{0, 11 * time.Minute, true},
		{720, 11 * time.Hour, false},
		{720, 13 * time.Hour, true},
	} {
		if got := (Automation{Grace: c.grace, NextRun: due}).missed(due.Add(c.late)); got != c.want {
			t.Errorf("grace %d, %v late: %v, want %v", c.grace, c.late, got, c.want)
		}
	}
	s := &store{dir: t.TempDir()}
	a, err := s.save(saveInput{Name: "n", Prompt: "p", Dir: t.TempDir(), Kind: "interval", Every: 5}, due)
	if err != nil || a.Grace != 10 {
		t.Errorf("saved grace %d %v, want 10", a.Grace, err)
	}
}
