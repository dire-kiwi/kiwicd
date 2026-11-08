package wf

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchedules(t *testing.T) {
	w, err := Parse([]byte(`on:
  push:
  schedule:
    - cron: '0 5 * * 1-5'
    - cron: '30 9 * * *'
      timezone: Pacific/Auckland
    - cron: '0 5 * * 1-5'
jobs:
  a:
    runs-on: x
    steps: [{run: echo}]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Schedule{{Cron: "0 5 * * 1-5"}, {Cron: "30 9 * * *", Timezone: "Pacific/Auckland"}}
	if got := Schedules(w); !reflect.DeepEqual(got, want) {
		t.Fatalf("Schedules = %+v, want %+v", got, want)
	}
	w, _ = Parse([]byte("on: push\njobs:\n  a:\n    runs-on: x\n    steps: [{run: echo}]\n"))
	if got := Schedules(w); got != nil {
		t.Fatalf("Schedules without schedule = %+v", got)
	}
}

func TestCronParse(t *testing.T) {
	for _, s := range []Schedule{
		{Cron: "*/5 * * * *"},
		{Cron: "0 0 1 JAN,JUL MON-FRI"},
		{Cron: "15 3 * * 0", Timezone: "America/New_York"},
	} {
		if _, err := s.Parse(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	for s, want := range map[Schedule]string{
		{Cron: "@hourly"}:                             "five fields",
		{Cron: "CRON_TZ=UTC 0 * * * *"}:               "timezone key",
		{Cron: "0 * * *"}:                             "five fields",
		{Cron: "0 0 * * * *"}:                         "five fields",
		{Cron: "61 * * * *"}:                          "invalid cron",
		{Cron: "0 * * * *", Timezone: "Mars/Olympus"}: "unknown timezone",
	} {
		if _, err := s.Parse(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", s, err, want)
		}
	}
}

func mustCron(t *testing.T, s Schedule) *CronSchedule {
	t.Helper()
	c, err := s.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronDue(t *testing.T) {
	c := mustCron(t, Schedule{Cron: "0 * * * *"})
	// Not run yet: the first slot after it was seen.
	if got := c.Due(at("2026-10-31T12:58:00Z"), time.Time{}); !got.Equal(at("2026-10-31T13:00:00Z")) {
		t.Errorf("first due = %v", got)
	}
	// A slot seen exactly isn't run: it was already under way.
	if got := c.Due(at("2026-10-31T13:00:00Z"), time.Time{}); !got.Equal(at("2026-10-31T14:00:00Z")) {
		t.Errorf("due when seen at a slot = %v", got)
	}
	if got := c.Due(time.Time{}, at("2026-10-31T13:00:00Z")); !got.Equal(at("2026-10-31T14:00:00Z")) {
		t.Errorf("due after 13:00 = %v", got)
	}

	// Every minute runs every five minutes at most.
	c = mustCron(t, Schedule{Cron: "* * * * *"})
	if got := c.Due(time.Time{}, at("2026-10-31T13:01:00Z")); !got.Equal(at("2026-10-31T13:06:00Z")) {
		t.Errorf("every minute: due = %v", got)
	}
	c = mustCron(t, Schedule{Cron: "0,2,4,10 * * * *"})
	if got := c.Due(time.Time{}, at("2026-10-31T13:00:00Z")); !got.Equal(at("2026-10-31T13:10:00Z")) {
		t.Errorf("0,2,4,10: due = %v", got)
	}
}

func TestCronTimezone(t *testing.T) {
	c := mustCron(t, Schedule{Cron: "30 9 * * *", Timezone: "Pacific/Auckland"})
	// NZDT is UTC+13 on 31 October 2026.
	if got := c.Next(at("2026-10-31T00:00:00Z")); !got.Equal(at("2026-10-31T20:30:00Z")) {
		t.Errorf("Next = %v", got.UTC())
	}
	// And UTC+12 (NZST) in June.
	if got := c.Next(at("2026-06-01T00:00:00Z")); !got.Equal(at("2026-06-01T21:30:00Z")) {
		t.Errorf("Next in winter = %v", got.UTC())
	}
	// Without one, UTC.
	c = mustCron(t, Schedule{Cron: "30 9 * * *"})
	if got := c.Next(at("2026-10-31T00:00:00Z")); !got.Equal(at("2026-10-31T09:30:00Z")) {
		t.Errorf("Next in UTC = %v", got)
	}
}

func TestCronLatest(t *testing.T) {
	c := mustCron(t, Schedule{Cron: "*/15 * * * *"})
	if got := c.Latest(at("2026-10-31T13:00:00Z"), at("2026-10-31T13:10:00Z")); !got.Equal(at("2026-10-31T13:00:00Z")) {
		t.Errorf("Latest of one = %v", got)
	}
	if got := c.Latest(at("2026-10-31T13:00:00Z"), at("2026-11-02T08:52:00Z")); !got.Equal(at("2026-11-02T08:45:00Z")) {
		t.Errorf("Latest after an outage = %v", got)
	}
}
