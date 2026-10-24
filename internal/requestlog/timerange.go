package requestlog

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	relativeTimePattern = regexp.MustCompile(`^([1-9][0-9]*)([mhdwMy])$`)
	datePattern         = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	timestampPattern    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{2}-\d{2}$`)
)

const maxCalendarRelativeAmount = 100_000

var maxRelativeDuration = time.Duration(1<<63 - 1)

// TimeRange is a half-open interval [Start, End). All means no range filter.
type TimeRange struct {
	Start time.Time
	End   time.Time
	All   bool
}

// ParseTimeRange parses --time. Date-only end bounds include the whole end
// date; minute-precision end bounds include the whole end minute.
func ParseTimeRange(value string, now time.Time) (TimeRange, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return TimeRange{All: true}, nil
	}
	if match := relativeTimePattern.FindStringSubmatch(value); match != nil {
		amount, err := strconv.Atoi(match[1])
		if err != nil {
			return TimeRange{}, fmt.Errorf("parse relative time range: %w", err)
		}
		end := now
		localNow := now.In(time.Local)
		var start time.Time
		var rangeErr error
		switch match[2] {
		case "m":
			start, rangeErr = subtractRelativeDuration(end, amount, time.Minute)
		case "h":
			start, rangeErr = subtractRelativeDuration(end, amount, time.Hour)
		case "d":
			start, rangeErr = subtractRelativeDuration(end, amount, 24*time.Hour)
		case "w":
			start, rangeErr = subtractRelativeDuration(end, amount, 7*24*time.Hour)
		case "M":
			start, rangeErr = subtractCalendarMonths(localNow, amount)
		case "y":
			start, rangeErr = subtractCalendarYears(localNow, amount)
		}
		if rangeErr != nil {
			return TimeRange{}, rangeErr
		}
		return TimeRange{Start: start, End: end.Add(time.Millisecond)}, nil
	}

	parts := strings.Split(value, ",")
	if len(parts) != 2 {
		return TimeRange{}, errors.New("time range must be START,END or a relative value such as 2m/1h/2d/1w/3M/1y (m: minutes, M: calendar months)")
	}
	start, err := parseTimeBoundary(strings.TrimSpace(parts[0]), false)
	if err != nil {
		return TimeRange{}, fmt.Errorf("parse start time: %w", err)
	}
	end, err := parseTimeBoundary(strings.TrimSpace(parts[1]), true)
	if err != nil {
		return TimeRange{}, fmt.Errorf("parse end time: %w", err)
	}
	if !end.After(start) {
		return TimeRange{}, errors.New("end time must be after start time")
	}
	return TimeRange{Start: start, End: end}, nil
}

func subtractRelativeDuration(end time.Time, amount int, unit time.Duration) (time.Time, error) {
	if unit <= 0 || time.Duration(amount) > maxRelativeDuration/unit {
		return time.Time{}, errors.New("relative time range is too large")
	}
	return end.Add(-time.Duration(amount) * unit), nil
}

func subtractCalendarMonths(value time.Time, amount int) (time.Time, error) {
	if amount > maxCalendarRelativeAmount {
		return time.Time{}, errors.New("relative time range is too large")
	}
	monthIndex := int(value.Month()) - 1 - amount
	yearOffset := monthIndex / 12
	monthIndex %= 12
	if monthIndex < 0 {
		monthIndex += 12
		yearOffset--
	}
	return clampToCalendarMonth(value, value.Year()+yearOffset, time.Month(monthIndex+1)), nil
}

func subtractCalendarYears(value time.Time, amount int) (time.Time, error) {
	if amount > maxCalendarRelativeAmount {
		return time.Time{}, errors.New("relative time range is too large")
	}
	return clampToCalendarMonth(value, value.Year()-amount, value.Month()), nil
}

func clampToCalendarMonth(value time.Time, year int, month time.Month) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, value.Location()).Day()
	day := value.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), value.Location())
}

func parseTimeBoundary(value string, isEnd bool) (time.Time, error) {
	switch {
	case datePattern.MatchString(value):
		parsed, err := time.ParseInLocation("2006-01-02", value, time.Local)
		if err != nil {
			return time.Time{}, err
		}
		if isEnd {
			parsed = parsed.AddDate(0, 0, 1)
		}
		return parsed, nil
	case timestampPattern.MatchString(value):
		parsed, err := time.ParseInLocation("2006-01-02-15-04", value, time.Local)
		if err != nil {
			return time.Time{}, err
		}
		if isEnd {
			parsed = parsed.Add(time.Minute)
		}
		return parsed, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported time %q", value)
	}
}
