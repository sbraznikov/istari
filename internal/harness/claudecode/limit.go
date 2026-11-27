package claudecode

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Claude Code's text when the account's usage limit is reached, e.g.
// "You've hit your 5-hour limit · resets 10pm (Europe/Berlin)". The reset is
// a time of day with an optional zone.
var (
	limitText = regexp.MustCompile(`(?i)^\s*you['’]ve hit your\b.*\blimit\b`)
	resetText = regexp.MustCompile(`(?i)\bresets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)
)

// usageLimit reports whether text is the usage-limit message, and when the
// limit resets: the next occurrence of the named time of day after now, in
// the named zone (now's zone when none is named). The reset is zero when the
// text names no time it can read.
func usageLimit(text string, now time.Time) (reset time.Time, ok bool) {
	if !limitText.MatchString(text) {
		return time.Time{}, false
	}
	m := resetText.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, true
	}
	hour, _ := strconv.Atoi(m[1]) // the pattern matched digits
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2]) // the pattern matched digits
	}
	switch strings.ToLower(m[3]) {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 12 {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}, true
	}
	loc := now.Location()
	if m[4] != "" {
		l, err := time.LoadLocation(strings.TrimSpace(m[4]))
		if err != nil {
			return time.Time{}, true // a zone it does not know: no time to trust
		}
		loc = l
	}
	local := now.In(loc)
	reset = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !reset.After(now) {
		reset = time.Date(local.Year(), local.Month(), local.Day()+1, hour, minute, 0, 0, loc)
	}
	return reset, true
}
