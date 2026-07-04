// Package croncal translates 5-field cron expressions into systemd
// OnCalendar expressions. It supports numerics, month/weekday names,
// '*', steps (*/6), ranges (1-5) and lists (1,3,5).
//
// Cron ORs day-of-month and day-of-week when both are restricted;
// systemd ANDs them. Rather than silently changing semantics, that
// combination is rejected.
package croncal

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// systemd weekday names, indexed by cron day-of-week number (0 = Sunday).
var dowOut = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// Translate converts a 5-field cron expression (minute hour day-of-month
// month day-of-week) into a systemd OnCalendar expression.
func Translate(expr string) (string, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", fmt.Errorf("cron expression %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}

	minutes, err := expand(fields[0], 0, 59, nil, "minute")
	if err != nil {
		return "", err
	}
	hours, err := expand(fields[1], 0, 23, nil, "hour")
	if err != nil {
		return "", err
	}
	days, err := expand(fields[2], 1, 31, nil, "day-of-month")
	if err != nil {
		return "", err
	}
	months, err := expand(fields[3], 1, 12, monthNames, "month")
	if err != nil {
		return "", err
	}
	dows, err := expand(fields[4], 0, 7, dowNames, "day-of-week")
	if err != nil {
		return "", err
	}

	if days != nil && dows != nil {
		return "", fmt.Errorf("cron expression %q restricts both day-of-month and day-of-week: cron ORs them but systemd ANDs them, so the schedule cannot be translated faithfully", expr)
	}

	var parts []string
	if dows != nil {
		var names []string
		seen := map[string]bool{}
		for _, d := range dows {
			name := dowOut[d%7] // cron allows 7 for Sunday
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		parts = append(parts, strings.Join(names, ","))
	}
	parts = append(parts, fmt.Sprintf("*-%s-%s", join(months, 2), join(days, 2)))
	parts = append(parts, fmt.Sprintf("%s:%s:00", join(hours, 2), join(minutes, 2)))
	return strings.Join(parts, " "), nil
}

// join renders values as a zero-padded comma list, or "*" for unrestricted.
func join(vals []int, width int) string {
	if vals == nil {
		return "*"
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%0*d", width, v)
	}
	return strings.Join(parts, ",")
}

// expand parses one cron field into a sorted, deduplicated value list.
// A nil result means the field is unrestricted ("*").
func expand(field string, min, max int, names map[string]int, what string) ([]int, error) {
	if field == "*" {
		return nil, nil
	}
	seen := map[int]bool{}
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return nil, fmt.Errorf("%s field %q: empty list item", what, field)
		}
		lo, hi, step := 0, 0, 1
		rangePart := item
		if i := strings.Index(item, "/"); i != -1 {
			rangePart = item[:i]
			s, err := strconv.Atoi(item[i+1:])
			if err != nil || s < 1 {
				return nil, fmt.Errorf("%s field %q: invalid step in %q", what, field, item)
			}
			step = s
		}
		switch {
		case rangePart == "*":
			lo, hi = min, max
		case strings.Contains(rangePart, "-"):
			parts := strings.SplitN(rangePart, "-", 2)
			var err error
			if lo, err = parseValue(parts[0], min, max, names, what, field); err != nil {
				return nil, err
			}
			if hi, err = parseValue(parts[1], min, max, names, what, field); err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("%s field %q: range %q is reversed", what, field, rangePart)
			}
		default:
			v, err := parseValue(rangePart, min, max, names, what, field)
			if err != nil {
				return nil, err
			}
			lo, hi = v, v
			if step != 1 {
				hi = max // "5/10" means every 10 starting at 5
			}
		}
		for v := lo; v <= hi; v += step {
			seen[v] = true
		}
	}
	vals := make([]int, 0, len(seen))
	for v := range seen {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	return vals, nil
}

func parseValue(s string, min, max int, names map[string]int, what, field string) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s field %q: invalid value %q", what, field, s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%s field %q: value %d out of range %d-%d", what, field, v, min, max)
	}
	return v, nil
}
