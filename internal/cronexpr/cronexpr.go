package cronexpr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a 5-field cron expression (minute hour day-of-month month day-of-week).
type Spec struct {
	minute field
	hour   field
	dom    field
	month  field
	dow    field
}

// Parse parses a 5-field cron expression.
func Parse(expr string) (Spec, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf("expected 5 cron fields, got %d", len(parts))
	}

	minute, err := parseField(parts[0], 0, 59, false)
	if err != nil {
		return Spec{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseField(parts[1], 0, 23, false)
	if err != nil {
		return Spec{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseField(parts[2], 1, 31, false)
	if err != nil {
		return Spec{}, fmt.Errorf("day-of-month: %w", err)
	}
	month, err := parseField(parts[3], 1, 12, false)
	if err != nil {
		return Spec{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseField(parts[4], 0, 7, true)
	if err != nil {
		return Spec{}, fmt.Errorf("day-of-week: %w", err)
	}

	return Spec{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// Match returns true when the provided local time matches the expression.
func (s Spec) Match(t time.Time) bool {
	if !s.minute.match(t.Minute()) || !s.hour.match(t.Hour()) || !s.month.match(int(t.Month())) {
		return false
	}
	domAny := s.dom.all
	dowAny := s.dow.all
	domMatch := s.dom.match(t.Day())
	dow := int(t.Weekday())
	dowMatch := s.dow.match(dow)

	// Cron semantics: when both dom and dow are restricted, either may match.
	if domAny && dowAny {
		return true
	}
	if domAny {
		return dowMatch
	}
	if dowAny {
		return domMatch
	}
	return domMatch || dowMatch
}

type field struct {
	all     bool
	allowed map[int]struct{}
}

func (f field) match(v int) bool {
	if f.all {
		return true
	}
	_, ok := f.allowed[v]
	return ok
}

func parseField(raw string, min, max int, normalizeDow bool) (field, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return field{}, fmt.Errorf("empty field")
	}
	if raw == "*" {
		return field{all: true}, nil
	}
	allowed := map[int]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return field{}, fmt.Errorf("invalid list element")
		}
		if err := parsePart(part, min, max, normalizeDow, allowed); err != nil {
			return field{}, err
		}
	}
	if len(allowed) == 0 {
		return field{}, fmt.Errorf("no values selected")
	}
	return field{allowed: allowed}, nil
}

func parsePart(part string, min, max int, normalizeDow bool, allowed map[int]struct{}) error {
	base := part
	step := 1
	if strings.Contains(part, "/") {
		segs := strings.Split(part, "/")
		if len(segs) != 2 {
			return fmt.Errorf("invalid step segment %q", part)
		}
		base = strings.TrimSpace(segs[0])
		rawStep := strings.TrimSpace(segs[1])
		n, err := strconv.Atoi(rawStep)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid step %q", rawStep)
		}
		step = n
	}

	if base == "*" {
		for v := min; v <= max; v += step {
			allowed[normalizeValue(v, normalizeDow)] = struct{}{}
		}
		return nil
	}

	start := 0
	end := 0
	if strings.Contains(base, "-") {
		segs := strings.Split(base, "-")
		if len(segs) != 2 {
			return fmt.Errorf("invalid range %q", base)
		}
		a, err := parseIntInRange(strings.TrimSpace(segs[0]), min, max)
		if err != nil {
			return err
		}
		b, err := parseIntInRange(strings.TrimSpace(segs[1]), min, max)
		if err != nil {
			return err
		}
		if b < a {
			return fmt.Errorf("invalid descending range %q", base)
		}
		start = a
		end = b
	} else {
		v, err := parseIntInRange(base, min, max)
		if err != nil {
			return err
		}
		start = v
		end = v
	}

	for v := start; v <= end; v += step {
		allowed[normalizeValue(v, normalizeDow)] = struct{}{}
	}
	return nil
}

func parseIntInRange(raw string, min, max int) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", raw)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d out of range [%d,%d]", n, min, max)
	}
	return n, nil
}

func normalizeValue(v int, normalizeDow bool) int {
	if normalizeDow && v == 7 {
		return 0
	}
	return v
}
