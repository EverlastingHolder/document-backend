package passport

import (
	"regexp"
	"strings"
	"time"
)

var (
	dateDot   = regexp.MustCompile(`^(\d{2})\.(\d{2})\.(\d{4})$`)
	dateDash  = regexp.MustCompile(`^(\d{2})-(\d{2})-(\d{4})$`)
	deptDash  = regexp.MustCompile(`^\d{3}-\d{3}$`)
	digitRun6 = regexp.MustCompile(`^\d{6}$`)
)

func parseDate(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	for _, pattern := range []*regexp.Regexp{dateDot, dateDash} {
		matches := pattern.FindStringSubmatch(value)
		if len(matches) != 4 {
			continue
		}
		parsed, err := time.Parse("02-01-2006", matches[1]+"-"+matches[2]+"-"+matches[3])
		if err != nil {
			return "", false
		}
		return parsed.Format("2006-01-02"), true
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.Format("2006-01-02"), true
	}
	return "", false
}

func canonicalGender(value string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "М", "МУЖ", "МУЖСКОЙ", "M":
		return "M", true
	case "Ж", "ЖЕН", "ЖЕНСКИЙ", "F":
		return "F", true
	default:
		return "", false
	}
}

func canonicalDepartment(value string) string {
	value = strings.TrimSpace(value)
	if deptDash.MatchString(value) {
		return value
	}
	var digits strings.Builder
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '\t' || r == '-' || r == '‐' || r == '‑':
		default:
			return value
		}
	}
	canonical := digits.String()
	if digitRun6.MatchString(canonical) {
		return canonical[:3] + "-" + canonical[3:]
	}
	return value
}
