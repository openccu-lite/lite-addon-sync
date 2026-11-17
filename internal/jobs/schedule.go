package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// The schedule of a job is a systemd timer on the system, which the Services page edits in full.
// The job dialog offers four choices and writes exactly the text the Services page's own form
// writes for them, so a timer the addon wrote opens there as "daily at 03:00" and follows that
// form. A text the form's shape does not explain was edited there: the dialog then shows "custom"
// and leaves it alone.

// The kinds of schedule.
const (
	Manual = "manual" // the timer is switched off; its files stay, so the job can be run by hand
	Hourly = "hourly"
	Daily  = "daily"
	Weekly = "weekly"
	Custom = "custom" // edited on the Services page; never written by the addon
)

// Weekdays as systemd writes them.
var Weekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// Schedule is a job's schedule as the dialog has it.
type Schedule struct {
	Kind string `json:"kind"`
	// Time is HH:MM, for daily and weekly.
	Time string `json:"time,omitempty"`
	// Weekday is Mon … Sun, for weekly.
	Weekday string `json:"weekday,omitempty"`
	// Calendar are the OnCalendar= lines of a custom timer (read-only).
	Calendar []string `json:"calendar,omitempty"`
}

var timeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (s Schedule) check() error {
	switch s.Kind {
	case Manual, Hourly, Custom:
		return nil
	case Daily, Weekly:
		if !timeRe.MatchString(s.Time) {
			return invalid("schedule: a time as HH:MM")
		}
		if s.Kind == Weekly && !isWeekday(s.Weekday) {
			return invalid("schedule: a weekday (Mon … Sun)")
		}
		return nil
	}
	return invalid("schedule: manual, hourly, daily or weekly")
}

func isWeekday(d string) bool {
	for _, w := range Weekdays {
		if w == d {
			return true
		}
	}
	return false
}

// stored is the schedule without what does not belong to its kind.
func (s Schedule) stored() Schedule {
	switch s.Kind {
	case Daily:
		return Schedule{Kind: Daily, Time: s.Time}
	case Weekly:
		return Schedule{Kind: Weekly, Time: s.Time, Weekday: s.Weekday}
	}
	return Schedule{Kind: s.Kind}
}

// Calendar is the OnCalendar= expression of a simple schedule; "" for manual and custom.
func (s Schedule) calendar() string {
	switch s.Kind {
	case Hourly:
		return "hourly"
	case Daily:
		return "*-*-* " + s.Time + ":00"
	case Weekly:
		return s.Weekday + " *-*-* " + s.Time + ":00"
	}
	return ""
}

// TimerText is the timer file of a simple schedule, as the Services page's form writes it
// (lib/timers.ts timerText): the description is the own timer's name there, which is sync-<id>.
// A manual job gets the text of a daily run at 03:00 - it is switched off, and switching it on
// later on the Services page gives a sensible schedule.
func TimerText(jobID string, s Schedule) string {
	if s.Kind != Hourly && s.Kind != Daily && s.Kind != Weekly {
		s = Schedule{Kind: Daily, Time: "03:00"}
	}
	lines := []string{"[Unit]", "Description=sync-" + jobID, "", "[Timer]", "OnCalendar=" + s.calendar(), "Persistent=true"}
	if s.Kind == Hourly {
		lines = append(lines, "RandomizedDelaySec=5min")
	}
	lines = append(lines, "", "[Install]", "WantedBy=timers.target", "")
	return strings.Join(lines, "\n")
}

// Hash is the SHA-256 of a timer text.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

var (
	dailyRe  = regexp.MustCompile(`^\*-\*-\* (\d{2}:\d{2}):00$`)
	weeklyRe = regexp.MustCompile(`^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) \*-\*-\* (\d{2}:\d{2}):00$`)
)

// directives lists the values of a key in a unit text.
func directives(text, key string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok && strings.TrimSpace(k) == key {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func normalize(text string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		out = append(out, strings.TrimRight(l, " \t\r"))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// ReadTimer reads a timer text back into a schedule: one of the simple ones when the text is what
// TimerText writes for it (with or without a random delay of any length - the Services page's
// form has that field), else Custom with its OnCalendar= lines. enabled false is Manual, whatever
// the text - but a custom text stays known as custom through Calendar.
func ReadTimer(jobID, text string, enabled bool) Schedule {
	s := Schedule{Kind: Custom, Calendar: directives(text, "OnCalendar")}
	if cal := s.Calendar; len(cal) == 1 {
		var simple Schedule
		switch {
		case cal[0] == "hourly":
			simple = Schedule{Kind: Hourly}
		case dailyRe.MatchString(cal[0]):
			simple = Schedule{Kind: Daily, Time: dailyRe.FindStringSubmatch(cal[0])[1]}
		case weeklyRe.MatchString(cal[0]):
			m := weeklyRe.FindStringSubmatch(cal[0])
			simple = Schedule{Kind: Weekly, Weekday: m[1], Time: m[2]}
		}
		if simple.Kind != "" && simple.check() == nil && sameShape(jobID, text, simple) {
			s = simple
		}
	}
	if !enabled {
		if s.Kind == Custom {
			return Schedule{Kind: Manual, Calendar: s.Calendar}
		}
		return Schedule{Kind: Manual}
	}
	return s
}

// sameShape says whether text is TimerText's text for s, allowing a RandomizedDelaySec= line of
// any value in the form's place for it (after Persistent=).
func sameShape(jobID, text string, s Schedule) bool {
	want := normalize(TimerText(jobID, Schedule{Kind: s.Kind, Time: s.Time, Weekday: s.Weekday}))
	got := normalize(text)
	if got == want {
		return true
	}
	// drop the delay line from both and compare
	strip := func(t string) string {
		var out []string
		for _, l := range strings.Split(t, "\n") {
			if !strings.HasPrefix(l, "RandomizedDelaySec=") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	return strip(got) == strip(want) && len(directives(text, "RandomizedDelaySec")) <= 1
}
