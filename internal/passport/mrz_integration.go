package passport

import (
	"math"
	"sort"
	"strings"
	"unicode"

	nationalmrz "passport-backend/internal/mrz"
)

type MRZDiagnostics struct {
	Status     string             `json:"status"`
	Candidates []MRZPairCandidate `json:"candidates"`
	Issues     []string           `json:"issues"`
}

type MRZPairCandidate struct {
	RawLines        [2]string          `json:"raw_lines"`
	RawVariants     [][2]string        `json:"raw_variants"`
	NormalizedLines [2]string          `json:"normalized_lines"`
	LineIDs         []string           `json:"line_ids"`
	PassIDs         []string           `json:"pass_ids"`
	Result          nationalmrz.Result `json:"result"`
}

type mrzLineGroup struct {
	pass        OCRPass
	lines       []locatedLine
	x, y, width float64
	docWidth    float64
}

type mrzPairDraft struct {
	lines   [2]string
	located []locatedLine
}

func collectAndMergeMRZ(input Observations, fields map[string]FieldResult) MRZDiagnostics {
	pairs := collectMRZPairs(input)
	byPair := make(map[[2]string]int)
	candidates := make([]MRZPairCandidate, 0, len(pairs))
	for _, pair := range pairs {
		normalized := [2]string{normalizeMRZVisual(pair.lines[0]), normalizeMRZVisual(pair.lines[1])}
		if normalized[0] == "" || normalized[1] == "" {
			continue
		}
		if index, ok := byPair[normalized]; ok {
			candidate := &candidates[index]
			candidate.RawVariants = appendMRZRawVariant(candidate.RawVariants, pair.lines)
			for _, line := range pair.located {
				candidate.LineIDs = appendUnique(candidate.LineIDs, line.line.ID)
				candidate.PassIDs = appendUnique(candidate.PassIDs, line.pass.ID)
			}
			continue
		}
		lineIDs, passIDs := []string{}, []string{}
		for _, line := range pair.located {
			lineIDs = appendUnique(lineIDs, line.line.ID)
			passIDs = appendUnique(passIDs, line.pass.ID)
		}
		parsed := nationalmrz.Parse(normalized)
		parsed.RawLines = pair.lines
		candidate := MRZPairCandidate{RawLines: pair.lines, RawVariants: [][2]string{pair.lines}, NormalizedLines: normalized, LineIDs: lineIDs, PassIDs: passIDs, Result: parsed}
		byPair[normalized] = len(candidates)
		candidates = append(candidates, candidate)
	}
	diagnostics := MRZDiagnostics{Status: mrzOverallStatus(candidates), Candidates: candidates, Issues: []string{}}
	if len(candidates) == 0 {
		diagnostics.Issues = append(diagnostics.Issues, "mrz_pair_not_found")
		return diagnostics
	}
	mergeMRZFields(fields, candidates)
	for _, candidate := range candidates {
		for _, issue := range candidate.Result.Issues {
			diagnostics.Issues = appendUnique(diagnostics.Issues, issue)
		}
	}
	return diagnostics
}

func mrzOverallStatus(candidates []MRZPairCandidate) string {
	valid, invalid, indeterminate := false, false, false
	for _, candidate := range candidates {
		switch candidate.Result.Status {
		case "valid":
			valid = true
		case "invalid":
			invalid = true
		default:
			indeterminate = true
		}
	}
	if valid {
		return "valid"
	}
	if indeterminate {
		return "indeterminate"
	}
	if invalid {
		return "invalid"
	}
	return "indeterminate"
}

func collectMRZPairs(input Observations) []mrzPairDraft {
	passes := make(map[string]OCRPass, len(input.Passes))
	for _, pass := range input.Passes {
		passes[pass.ID] = pass
	}
	groupsByPass := make(map[string][]mrzLineGroup)
	for _, line := range input.Lines {
		pass, ok := passes[line.PassID]
		if !ok || pass.Status != "ok" {
			continue
		}
		cx, cy := center(line.Quad)
		w := (distance(line.Quad[0], line.Quad[1]) + distance(line.Quad[2], line.Quad[3])) / 2
		if pass.Kind != "mrz" {
			if pass.Kind == "right_margin_cw" || pass.Kind == "right_margin_ccw" || cy < input.Document.Bounds[1]+0.67*docHeight(input) {
				continue
			}
		}
		groups := groupsByPass[pass.ID]
		merged := false
		for i := range groups {
			if abs(groups[i].y-cy) <= 0.018*docHeight(input) {
				groups[i].lines = append(groups[i].lines, locatedLine{line: line, pass: pass, x: cx, y: cy})
				groups[i].y = (groups[i].y*float64(len(groups[i].lines)-1) + cy) / float64(len(groups[i].lines))
				groups[i].width = groupWidth(groups[i].lines)
				merged = true
				break
			}
		}
		if !merged {
			groups = append(groups, mrzLineGroup{pass: pass, lines: []locatedLine{{line: line, pass: pass, x: cx, y: cy}}, x: cx, y: cy, width: w, docWidth: docWidth(input)})
		}
		groupsByPass[pass.ID] = groups
	}
	var pairs []mrzPairDraft
	for _, pass := range input.Passes {
		groups := groupsByPass[pass.ID]
		sort.Slice(groups, func(i, j int) bool { return groups[i].y < groups[j].y })
		for i := 0; i < len(groups); i++ {
			for j := i + 1; j < len(groups); j++ {
				verticalGap := groups[j].y - groups[i].y
				if verticalGap < 0.008*docHeight(input) || verticalGap > 0.16*docHeight(input) {
					continue
				}
				if groups[i].pass.Kind != "mrz" && (groups[i].width < 0.30*docWidth(input) || groups[j].width < 0.30*docWidth(input)) {
					continue
				}
				first, firstLocated := joinMRZGroup(groups[i])
				second, secondLocated := joinMRZGroup(groups[j])
				if first == "" || second == "" {
					continue
				}
				located := append(firstLocated, secondLocated...)
				pairs = append(pairs, mrzPairDraft{lines: [2]string{first, second}, located: located})
			}
		}
	}
	return pairs
}

func joinMRZGroup(group mrzLineGroup) (string, []locatedLine) {
	lines := append([]locatedLine(nil), group.lines...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].x < lines[j].x })
	for i := 1; i < len(lines); i++ {
		previousRight := maxQuadX(lines[i-1].line.Quad)
		currentLeft := minQuadX(lines[i].line.Quad)
		if currentLeft-previousRight > 0.04*group.docWidth {
			return "", nil
		}
	}
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.line.TextRaw)
	}
	return text.String(), lines
}

func groupWidth(lines []locatedLine) float64 {
	if len(lines) == 0 {
		return 0
	}
	minimum, maximum := minQuadX(lines[0].line.Quad), maxQuadX(lines[0].line.Quad)
	for _, line := range lines[1:] {
		minimum = math.Min(minimum, minQuadX(line.line.Quad))
		maximum = math.Max(maximum, maxQuadX(line.line.Quad))
	}
	return maximum - minimum
}

func minQuadX(quad Quad) float64 {
	minimum := quad[0][0]
	for _, point := range quad[1:] {
		minimum = math.Min(minimum, point[0])
	}
	return minimum
}
func maxQuadX(quad Quad) float64 {
	maximum := quad[0][0]
	for _, point := range quad[1:] {
		maximum = math.Max(maximum, point[0])
	}
	return maximum
}

func normalizeMRZVisual(raw string) string {
	var normalized strings.Builder
	for _, r := range raw {
		if unicode.IsSpace(r) {
			continue
		}
		if r >= 'а' && r <= 'я' || r == 'ё' {
			r = unicode.ToUpper(r)
		}
		switch r {
		case 'А':
			r = 'A'
		case 'В':
			r = 'B'
		case 'Е':
			r = 'E'
		case 'К':
			r = 'K'
		case 'М':
			r = 'M'
		case 'Н':
			r = 'H'
		case 'О':
			r = 'O'
		case 'Р':
			r = 'P'
		case 'С':
			r = 'C'
		case 'Т':
			r = 'T'
		case 'Х':
			r = 'X'
		case 'У':
			r = 'Y'
		case 'І', 'Ӏ':
			r = 'I'
		}
		if r > unicode.MaxASCII {
			return ""
		}
		r = unicode.ToUpper(r)
		normalized.WriteRune(r)
	}
	return normalized.String()
}

func mergeMRZFields(fields map[string]FieldResult, candidates []MRZPairCandidate) {
	valid := make([]MRZPairCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Result.Status == "valid" {
			valid = append(valid, candidate)
		}
	}
	if len(valid) == 0 {
		return
	}
	proposals := make(map[string]map[string]Candidate)
	for _, candidate := range valid {
		f := candidate.Result.Fields
		add := func(field, value, raw string) {
			if value == "" {
				return
			}
			if proposals[field] == nil {
				proposals[field] = map[string]Candidate{}
			}
			rule := "national_mrz_unchecked_value"
			switch field {
			case "series", "number", "department_code", "birth_date", "issue_date":
				rule = "national_mrz_checks_valid"
			case "surname", "given_name", "patronymic":
				rule = "national_mrz_unchecksummed_name"
			case "gender":
				rule = "national_mrz_unchecked_gender"
			}
			rawVariants := mrzRawVariantStrings(candidate)
			proposal := Candidate{Value: value, Raw: raw, RawVariants: rawVariants, Rules: []string{rule}, LineIDs: append([]string(nil), candidate.LineIDs...), PassIDs: append([]string(nil), candidate.PassIDs...), Context: "mrz:" + candidate.Result.Profile}
			if old, ok := proposals[field][value]; ok {
				proposals[field][value] = mergeCandidate(old, proposal)
			} else {
				proposals[field][value] = proposal
			}
		}
		raw := strings.Join(candidate.RawLines[:], "\n")
		rawVariants := mrzRawVariantStrings(candidate)
		add("series", f.Series, raw)
		add("number", f.Number, raw)
		add("department_code", formatDepartment(f.DepartmentCode), raw)
		add("gender", f.Gender, raw)
		if candidate.Result.NamesComplete {
			add("surname", f.Surname, raw)
			add("given_name", f.GivenName, raw)
			add("patronymic", f.Patronymic, raw)
		}
		for _, item := range []struct {
			name  string
			parts nationalmrz.DateParts
		}{{"birth_date", f.BirthDate}, {"issue_date", f.IssueDate}} {
			viz := fields[item.name]
			if item.parts.Raw != "" {
				appendPartialMRZDate(&viz, item.name, item.parts.Raw, raw, rawVariants, candidate.LineIDs, candidate.PassIDs)
				fields[item.name] = viz
			}
			if viz.Value != nil {
				if resolved, ok := nationalmrz.ResolveDate(item.parts, *viz.Value); ok {
					if item.parts.MonthKnown && item.parts.DayKnown {
						add(item.name, resolved, raw)
					} else {
						viz.Issues = appendUnique(viz.Issues, "mrz_partial_date_not_resolved")
						fields[item.name] = viz
					}
				} else if item.parts.Raw != "" {
					viz.Issues = appendUnique(viz.Issues, "mrz_viz_date_conflict")
					viz.State = "uncertain"
					viz.Value = nil
					fields[item.name] = viz
				}
			} else if item.parts.Raw != "" {
				viz.Issues = appendUnique(viz.Issues, "mrz_century_unresolved")
				viz.State = "uncertain"
				fields[item.name] = viz
			}
		}
	}
	for field, byValue := range proposals {
		values := make([]string, 0, len(byValue))
		for value := range byValue {
			values = append(values, value)
		}
		sort.Strings(values)
		viz := fields[field]
		for _, value := range values {
			viz.Candidates = appendCandidateUnique(viz.Candidates, byValue[value])
		}
		for _, value := range values {
			viz.Evidence.LineIDs = uniqueStrings(append(viz.Evidence.LineIDs, byValue[value].LineIDs...))
			viz.Evidence.PassIDs = uniqueStrings(append(viz.Evidence.PassIDs, byValue[value].PassIDs...))
		}
		vizConflict := false
		for _, existing := range viz.Candidates {
			if strings.HasPrefix(existing.Context, "mrz:partial:") {
				continue
			}
			if !containsCanonicalValue(field, values, existing.Value) {
				vizConflict = true
			}
		}
		vizAgreement := viz.Value != nil && containsCanonicalValue(field, values, *viz.Value)
		if hasIssue(viz.Issues, "mrz_viz_date_conflict") || len(values) > 1 || vizConflict || (viz.Value != nil && !vizAgreement) {
			viz.Value = nil
			viz.State = "uncertain"
			viz.Issues = appendUnique(viz.Issues, "mrz_field_conflict")
			fields[field] = viz
			continue
		}
		value := values[0]
		if vizAgreement {
			viz.Evidence.LineIDs = uniqueStrings(append(viz.Evidence.LineIDs, byValue[value].LineIDs...))
			viz.Evidence.PassIDs = uniqueStrings(append(viz.Evidence.PassIDs, byValue[value].PassIDs...))
			viz.Rules = uniqueStrings(append(viz.Rules, "mrz_viz_agreement"))
			fields[field] = viz
			continue
		}
		if viz.Value == nil && len(viz.Candidates) > 1 {
			viz.State = "uncertain"
			viz.Issues = appendUnique(viz.Issues, "mrz_viz_conflict")
			fields[field] = viz
			continue
		}
		v := value
		viz.Value = &v
		viz.State = "uncertain"
		viz.Raw = byValue[value].Raw
		viz.Rules = uniqueStrings(append(viz.Rules, "mrz_candidate_requires_manual_review"))
		viz.Context = byValue[value].Context
		viz.Issues = appendUnique(viz.Issues, "mrz_unchecked_or_manual_review_required")
		fields[field] = viz
	}
}

func appendPartialMRZDate(field *FieldResult, name, rawDate, rawLines string, rawVariants []string, lineIDs, passIDs []string) {
	partial := Candidate{
		Value: rawDate, Raw: rawLines, RawVariants: append([]string(nil), rawVariants...),
		Rules:   []string{"national_mrz_partial_date_no_century"},
		LineIDs: append([]string(nil), lineIDs...), PassIDs: append([]string(nil), passIDs...),
		Context: "mrz:partial:" + name,
	}
	for _, candidate := range field.Candidates {
		if candidate.Value == rawDate && candidate.Context == "mrz:partial:"+name {
			for i := range field.Candidates {
				if field.Candidates[i].Value == rawDate && field.Candidates[i].Context == "mrz:partial:"+name {
					field.Candidates[i] = mergeCandidate(field.Candidates[i], partial)
					return
				}
			}
		}
	}
	field.Candidates = append(field.Candidates, partial)
}

func appendMRZRawVariant(existing [][2]string, value [2]string) [][2]string {
	for _, candidate := range existing {
		if candidate == value {
			return existing
		}
	}
	return append(existing, value)
}

func mrzRawVariantStrings(candidate MRZPairCandidate) []string {
	variants := make([]string, 0, len(candidate.RawVariants))
	for _, pair := range candidate.RawVariants {
		variants = appendUnique(variants, strings.Join(pair[:], "\n"))
	}
	if len(variants) == 0 {
		variants = append(variants, strings.Join(candidate.RawLines[:], "\n"))
	}
	return variants
}

func appendCandidateUnique(values []Candidate, candidate Candidate) []Candidate {
	for i := range values {
		if values[i].Value == candidate.Value {
			values[i] = mergeCandidate(values[i], candidate)
			return values
		}
	}
	return append(values, candidate)
}

func formatDepartment(value string) string {
	if len(value) == 6 {
		return value[:3] + "-" + value[3:]
	}
	return value
}
func containsCanonicalValue(field string, values []string, wanted string) bool {
	for _, value := range values {
		if sameCanonicalFieldValue(field, value, wanted) {
			return true
		}
	}
	return false
}

func sameCanonicalFieldValue(field, left, right string) bool {
	canonical := func(value string) string {
		switch field {
		case "gender":
			switch strings.ToUpper(strings.TrimSpace(value)) {
			case "МУЖ.", "МУЖ", "МУЖСКОЙ", "М", "M":
				return "M"
			case "ЖЕН.", "ЖЕН", "ЖЕНСКИЙ", "Ж", "F":
				return "F"
			}
		case "department_code":
			return canonicalDepartment(value)
		}
		return value
	}
	return canonical(left) == canonical(right)
}
