package jobs

import "time"

// A job is overdue when it has a schedule and its last run that worked is older than twice its
// interval - three days for a timer edited on the Services page, whose interval the addon does not
// know. A job that never ran counts from when it was made. The system's Status page then says so
// (an addon notice), until a run works again.

// OverdueAfter is how long after its last good run a job with this schedule is overdue; 0 for a
// job without one.
func OverdueAfter(s Schedule) time.Duration {
	switch s.Kind {
	case Hourly:
		return 2 * time.Hour
	case Daily:
		return 48 * time.Hour
	case Weekly:
		return 14 * 24 * time.Hour
	case Custom:
		return 72 * time.Hour
	}
	return 0
}

// Overdue says since when the job's last good run is missing, when that is too long: schedule is
// the timer as the system has it now (Manual for a timer that is switched off), history the job's
// runs, newest first.
func Overdue(j Job, schedule Schedule, history []Record, now time.Time) (since time.Time, overdue bool) {
	limit := OverdueAfter(schedule)
	if limit == 0 || j.NeedsDryRun {
		return time.Time{}, false
	}
	since = j.Created
	if j.Changed.After(since) {
		since = j.Changed // a job that was just pointed elsewhere starts anew
	}
	for _, r := range history {
		if !r.DryRun && r.OK() {
			if r.End.After(since) {
				since = r.End
			}
			break
		}
	}
	return since, now.Sub(since) > limit
}
