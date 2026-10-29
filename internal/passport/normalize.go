package passport

import "strings"

var visualNameRunes = map[rune]rune{
	'A': 'А', 'B': 'В', 'C': 'С', 'E': 'Е', 'H': 'Н', 'K': 'К', 'M': 'М', 'O': 'О', 'P': 'Р', 'T': 'Т', 'X': 'Х', 'Y': 'У',
}

func normalizeName(raw string, enabled bool) (string, []string, bool) {
	if !enabled {
		return raw, nil, containsLatin(raw)
	}
	var out strings.Builder
	rules := make([]string, 0)
	seen := make(map[rune]bool)
	for _, r := range raw {
		mapped, ok := visualNameRunes[r]
		if ok {
			r = mapped
			if !seen[r] {
				rules = append(rules, "visual_latin_lookalike_to_cyrillic")
				seen[r] = true
			}
		}
		out.WriteRune(r)
	}
	value := out.String()
	return value, rules, len(rules) > 0 || containsLatin(value)
}

func containsLatin(s string) bool {
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return true
		}
	}
	return false
}
