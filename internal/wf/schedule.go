package wf

import (
	"fmt"
	"strings"
	"time"

	"github.com/nektos/act/pkg/model"
	"github.com/robfig/cron/v3"
)

// Schedule is one entry of a workflow's `on.schedule`:
//
//	on:
//	  schedule:
//	    - cron: '30 5 * * 1-5'
//	      timezone: Pacific/Auckland   # optional; UTC by default
type Schedule struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
}

// MinScheduleInterval is the shortest time between two runs of a schedule,
// as on GitHub: slots closer than this to the last one are skipped.
const MinScheduleInterval = 5 * time.Minute

// Schedules returns the workflow's schedule entries, without duplicates.
func Schedules(w *model.Workflow) []Schedule {
	if !HasEvent(w, "schedule") {
		return nil
	}
	list, _ := w.OnEvent("schedule").([]any)
	var out []Schedule
	seen := map[Schedule]bool{}
	for _, item := range list {
		m, _ := item.(map[string]any)
		c, _ := m["cron"].(string)
		tz, _ := m["timezone"].(string)
		s := Schedule{Cron: strings.TrimSpace(c), Timezone: strings.TrimSpace(tz)}
		if s.Cron == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// CronSchedule is a parsed Schedule.
type CronSchedule struct {
	spec cron.Schedule
	loc  *time.Location
}

// Parse checks a schedule: five POSIX cron fields (minute, hour, day of
// month, month, day of week; names like MON and JAN are allowed), and an
// IANA time zone if one is given. Like GitHub, it doesn't take @hourly and
// the other descriptors.
func (s Schedule) Parse() (*CronSchedule, error) {
	if strings.HasPrefix(s.Cron, "@") || strings.Contains(s.Cron, "TZ=") {
		return nil, fmt.Errorf("invalid cron %q: use five fields, and the timezone key for a time zone", s.Cron)
	}
	if n := len(strings.Fields(s.Cron)); n != 5 {
		return nil, fmt.Errorf("invalid cron %q: it needs five fields (minute hour day-of-month month day-of-week), not %d", s.Cron, n)
	}
	spec, err := cronParser.Parse(s.Cron)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %v", s.Cron, err)
	}
	loc := time.UTC
	if s.Timezone != "" {
		if loc, err = time.LoadLocation(s.Timezone); err != nil {
			return nil, fmt.Errorf("unknown timezone %q", s.Timezone)
		}
	}
	return &CronSchedule{spec: spec, loc: loc}, nil
}

// Next returns the first slot strictly after t.
func (c *CronSchedule) Next(t time.Time) time.Time {
	return c.spec.Next(t.In(c.loc))
}

// Due returns when the schedule next fires: the first slot after since if it
// hasn't fired yet (last is zero), otherwise the first slot at least
// MinScheduleInterval after the last one fired.
func (c *CronSchedule) Due(since, last time.Time) time.Time {
	if last.IsZero() {
		return c.Next(since)
	}
	return c.Next(last.Add(MinScheduleInterval - time.Second))
}

// Latest returns the last slot at or before now, starting from due (a slot
// at or before now). Slots missed while kiwicd wasn't running are skipped
// this way, rather than each starting a run.
func (c *CronSchedule) Latest(due, now time.Time) time.Time {
	slot := due
	for range 1_000_000 { // a year of every minute
		next := c.Next(slot)
		if next.After(now) || next.IsZero() {
			break
		}
		slot = next
	}
	return slot
}
