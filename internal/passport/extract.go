package passport

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	digitRun10 = regexp.MustCompile(`^\d{10}$`)
	digitRun6  = regexp.MustCompile(`^\d{6}$`)
	digitRun4  = regexp.MustCompile(`^\d{4}$`)
	deptDash   = regexp.MustCompile(`^\d{3}-\d{3}$`)
	dateDot    = regexp.MustCompile(`^(\d{2})\.(\d{2})\.(\d{4})$`)
	dateDash   = regexp.MustCompile(`^(\d{2})-(\d{2})-(\d{4})$`)
)

type locatedLine struct {
	line OCRLine
	pass OCRPass
	x    float64
	y    float64
}

type candidateDraft struct {
	value   string
	raw     string
	lines   []locatedLine
	rules   []string
	context string
}

var labelTerms = map[string][]string{
	"surname":         {"ФАМИЛИЯ"},
	"given_name":      {"ИМЯ"},
	"patronymic":      {"ОТЧЕСТВО"},
	"gender":          {"ПОЛ"},
	"birth_date":      {"ДАТАРОЖДЕНИЯ"},
	"birth_place":     {"МЕСТОРОЖДЕНИЯ"},
	"series":          {"СЕРИЯ"},
	"number":          {"НОМЕР"},
	"issue_date":      {"ДАТАВЫДАЧИ"},
	"issuer":          {"КЕМВЫДАН", "ОРГАНВЫДАВШИЙ"},
	"department_code": {"КОДПОДРАЗДЕЛЕНИЯ"},
}

func Extract(input Observations, options Options) (Extraction, error) {
	if err := input.Validate(); err != nil {
		return Extraction{}, err
	}
	if options.PassSet == "" {
		options.PassSet = "original"
	}
	if options.MRZPolicy == "" {
		options.MRZPolicy = "off"
	}
	if options.PassSet != "original" && options.PassSet != "all" {
		return Extraction{}, fmt.Errorf("unsupported pass set")
	}
	if options.MRZPolicy != "off" && options.MRZPolicy != "augment" {
		return Extraction{}, fmt.Errorf("unsupported MRZ policy")
	}
	lines := selectedLines(input, options.PassSet)
	semanticLines := semanticFieldLines(lines)
	fields := make(map[string]FieldResult, 11)
	for _, name := range []string{"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place", "series", "number", "issue_date", "issuer", "department_code"} {
		fields[name] = emptyField()
	}

	// Explicit labels anchor values. Layout fallbacks are proposals and always
	// remain uncertain; positions are relative to detected document bounds.
	for _, name := range []string{"surname", "given_name", "patronymic", "gender", "birth_date", "birth_place", "issue_date", "issuer", "department_code"} {
		fields[name] = extractLabeledField(input, semanticLines, name, options.NormalizeNames)
	}
	if fields["surname"].State == "missing" || fields["given_name"].State == "missing" || fields["patronymic"].State == "missing" {
		fallbackNames := extractLayoutNames(input, semanticLines, fields["birth_date"], options.NormalizeNames)
		for name, field := range fallbackNames {
			if fields[name].State == "missing" {
				fields[name] = field
			}
		}
	}
	if fields["birth_date"].State == "missing" {
		fallback := extractUnlabeledDate(input, semanticLines, false)
		if len(fallback.Candidates) > 0 || len(fields["birth_date"].Issues) == 0 {
			for _, issue := range fields["birth_date"].Issues {
				fallback.Issues = appendUnique(fallback.Issues, issue)
			}
			fields["birth_date"] = fallback
		}
	}
	if fields["issue_date"].State == "missing" {
		fallback := extractUnlabeledDate(input, semanticLines, true)
		if len(fallback.Candidates) > 0 || len(fields["issue_date"].Issues) == 0 {
			for _, issue := range fields["issue_date"].Issues {
				fallback.Issues = appendUnique(fallback.Issues, issue)
			}
			fields["issue_date"] = fallback
		}
	}
	if fields["gender"].State == "missing" && !hasFieldLabel(semanticLines, "gender") {
		fields["gender"] = extractLayoutGender(input, semanticLines, fields["birth_date"])
	}
	if fields["issuer"].State == "missing" && !hasFieldLabel(semanticLines, "issuer") {
		fields["issuer"] = extractLayoutIssuer(input, semanticLines, fields["issue_date"])
	}
	if fields["birth_place"].State == "missing" && !hasFieldLabel(semanticLines, "birth_place") {
		fields["birth_place"] = extractLayoutBirthPlace(input, semanticLines, fields["birth_date"])
	}
	if fields["department_code"].State == "missing" && !hasFieldLabel(semanticLines, "department_code") {
		fields["department_code"] = extractLayoutDepartmentCode(input, semanticLines, fields["issue_date"])
	}
	fields["series"], fields["number"] = extractSerialNumber(input, lines)
	result := Extraction{SchemaVersion: ExtractionSchema, ImageID: input.Image.ID, ReviewRequired: true, Fields: fields}
	if options.MRZPolicy == "augment" {
		mrzDiagnostics := collectAndMergeMRZ(input, fields)
		result.MRZ = &mrzDiagnostics
	}
	return result, nil
}

func hasFieldLabel(lines []locatedLine, field string) bool {
	for _, line := range lines {
		if hasAnyLabel(line.line.TextRaw, labelTerms[field]) {
			return true
		}
	}
	return false
}

func selectedLines(input Observations, passSet string) []locatedLine {
	passes := make(map[string]OCRPass, len(input.Passes))
	for _, p := range input.Passes {
		passes[p.ID] = p
	}
	out := make([]locatedLine, 0, len(input.Lines))
	for _, l := range input.Lines {
		p, ok := passes[l.PassID]
		if !ok || p.Status != "ok" || p.Kind == "mrz" || (passSet == "original" && p.Kind != "original") {
			continue
		}
		cx, cy := center(l.Quad)
		out = append(out, locatedLine{line: l, pass: p, x: cx, y: cy})
	}
	return out
}

func semanticFieldLines(lines []locatedLine) []locatedLine {
	// Rotated right-margin passes supply serial evidence, not semantic fields.
	semantic := make([]locatedLine, 0, len(lines))
	for _, line := range lines {
		if line.pass.Kind == "right_margin_cw" || line.pass.Kind == "right_margin_ccw" {
			continue
		}
		semantic = append(semantic, line)
	}
	return semantic
}

func extractLabeledField(input Observations, lines []locatedLine, field string, normalizeNames bool) FieldResult {
	anchors := make([]locatedLine, 0)
	terms := labelTerms[field]
	for _, line := range lines {
		if hasAnyLabel(line.line.TextRaw, terms) {
			anchors = append(anchors, line)
		}
	}
	drafts := make([]candidateDraft, 0)
	for _, anchor := range anchors {
		valuePart := stripLabel(anchor.line.TextRaw, terms)
		if field == "issuer" || field == "birth_place" {
			blockDrafts := collectLabeledBlock(input, lines, anchor, field)
			if len(blockDrafts) > 0 {
				drafts = append(drafts, blockDrafts...)
			} else if usableText(valuePart) {
				drafts = append(drafts, candidateDraft{value: cleanValue(valuePart), raw: anchor.line.TextRaw, lines: []locatedLine{anchor}, rules: []string{"explicit_label_inline_value"}, context: "explicit_label:inline"})
			}
			continue
		}
		if usableText(valuePart) {
			value := cleanValue(valuePart)
			if value != "" {
				drafts = append(drafts, candidateDraft{value: value, raw: anchor.line.TextRaw, lines: []locatedLine{anchor}, rules: []string{"explicit_label_inline_value"}, context: "explicit_label:inline"})
			}
		}
		if usableText(valuePart) {
			continue
		}
		for _, near := range nearbyValueLines(input, lines, anchor) {
			value := canonicalFieldValue(field, near.line.TextRaw)
			if value != "" {
				drafts = append(drafts, candidateDraft{value: value, raw: anchor.line.TextRaw + "\n" + near.line.TextRaw, lines: []locatedLine{anchor, near}, rules: []string{"explicit_label_spatial_association"}, context: "explicit_label:spatial_neighbor"})
			}
		}
	}
	if field == "series" || field == "number" {
		// The visible page often has no explicit text label adjacent to the
		// value; numeric fields are handled by their stricter layout parser.
		return emptyField()
	}
	return resolveDrafts(input, field, drafts, false, normalizeNames)
}

func collectLabeledBlock(input Observations, lines []locatedLine, anchor locatedLine, field string) []candidateDraft {
	page := pageOf(input, anchor.y)
	if page == "unknown" {
		return nil
	}
	terms := labelTerms[field]
	allTerms := allLabelTerms()
	var nextLabelY float64
	hasNextLabel := false
	for _, line := range lines {
		if line.line.ID == anchor.line.ID || line.pass.ID != anchor.pass.ID || pageOf(input, line.y) != page || line.y <= anchor.y || !hasAnyLabel(line.line.TextRaw, allTerms) {
			continue
		}
		if line.x < anchor.x-0.04*docWidth(input) || line.x > anchor.x+0.62*docWidth(input) {
			continue
		}
		if !hasNextLabel || line.y < nextLabelY {
			nextLabelY = line.y
			hasNextLabel = true
		}
	}
	block := []locatedLine{anchor}
	for _, line := range lines {
		if line.line.ID == anchor.line.ID || line.pass.ID != anchor.pass.ID || pageOf(input, line.y) != page || !hasLetters(line.line.TextRaw) || hasAnyLabel(line.line.TextRaw, allTerms) {
			continue
		}
		if line.y <= anchor.y || line.y-anchor.y > 0.12*docHeight(input) {
			continue
		}
		if hasNextLabel && line.y >= nextLabelY {
			continue
		}
		if line.x < anchor.x-0.04*docWidth(input) || line.x > anchor.x+0.62*docWidth(input) {
			continue
		}
		block = append(block, line)
	}
	if len(block) < 2 {
		return nil
	}
	// Spatial order, never OCR source order. Keep only a contiguous block of
	// up to four rows; large gaps signal a new field or unrelated text.
	sort.Slice(block[1:], func(i, j int) bool {
		a, b := block[1+i], block[1+j]
		if a.y == b.y {
			return a.x < b.x
		}
		return a.y < b.y
	})
	selected := []locatedLine{anchor}
	lastY := anchor.y
	for _, line := range block[1:] {
		if len(selected) == 5 || line.y-lastY > 0.045*docHeight(input) {
			break
		}
		selected = append(selected, line)
		lastY = line.y
	}
	if len(selected) < 2 {
		return nil
	}
	valueParts := make([]string, 0, len(selected))
	for i, line := range selected {
		part := line.line.TextRaw
		if i == 0 {
			part = stripLabel(part, terms)
		}
		part = cleanValue(part)
		if part != "" {
			valueParts = append(valueParts, part)
		}
	}
	if len(valueParts) == 0 {
		return nil
	}
	return []candidateDraft{{value: strings.Join(valueParts, " "), raw: joinRaw(selected), lines: selected, rules: []string{"join_adjacent_labeled_text_rows"}, context: "explicit_label:multiline_spatial_block"}}
}

func extractLayoutNames(input Observations, lines []locatedLine, birthDate FieldResult, normalizeNames bool) map[string]FieldResult {
	result := make(map[string]FieldResult)
	lower := pageSeam(input)
	upperBound := input.Document.Bounds[3]
	if birthDate.Evidence.LineIDs != nil && len(birthDate.Evidence.LineIDs) > 0 {
		for _, line := range lines {
			if line.line.ID == birthDate.Evidence.LineIDs[0] {
				upperBound = line.y
				break
			}
		}
	} else {
		upperBound = lower + 0.48*docHeight(input)
	}
	rightStart := input.Document.Bounds[0] + 0.42*docWidth(input)
	byPass := make(map[string][]locatedLine)
	for _, line := range lines {
		if pageOf(input, line.y) != "lower" || line.y < lower+0.035*docHeight(input) || line.y >= upperBound || line.x < rightStart || !nameLike(line.line.TextRaw) || hasAnyLabel(line.line.TextRaw, []string{"ПАСПОРТ", "РОССИЙСКОЙ", "ФЕДЕРАЦИИ", "ФАМИЛИЯ", "ИМЯ", "ОТЧЕСТВО"}) {
			continue
		}
		byPass[line.pass.ID] = append(byPass[line.pass.ID], line)
	}
	fieldNames := []string{"surname", "given_name", "patronymic"}
	draftsByField := make(map[string][]candidateDraft)
	passIDs := make([]string, 0, len(byPass))
	for passID := range byPass {
		passIDs = append(passIDs, passID)
	}
	sort.Strings(passIDs)
	for _, passID := range passIDs {
		rows := groupRows(byPass[passID], 0.018*docHeight(input))
		for i, row := range rows {
			if i >= len(fieldNames) {
				break
			}
			fieldName := fieldNames[i]
			d := candidateDraft{value: cleanValue(joinRaw(row)), raw: joinRaw(row), lines: row, rules: []string{"lower_page_name_row_order_fallback"}, context: "layout_fallback:lower_page_name_rows_in_reading_order"}
			draftsByField[fieldName] = append(draftsByField[fieldName], d)
		}
	}
	for _, fieldName := range fieldNames {
		if len(draftsByField[fieldName]) > 0 {
			result[fieldName] = resolveDrafts(input, fieldName, draftsByField[fieldName], true, normalizeNames)
		}
	}
	return result
}

// These document-relative thresholds define bounded proposal regions only;
// they are heuristics, not calibrated physical passport measurements.
const (
	_layoutRightOfPhoto      = 0.42
	_layoutGenderZoneOverlap = 0.25
	_layoutRightMargin       = 0.70
	_layoutNearDateY         = 0.10
	_layoutMaxRowGap         = 0.045
	_layoutMaxBlockHeight    = 0.16
	_layoutMRZStart          = 0.86
	_layoutWideLineFraction  = 0.55
)

func evidenceAnchor(lines []locatedLine, field FieldResult) (locatedLine, bool) {
	if len(field.Candidates) != 1 || len(field.Candidates[0].LineIDs) == 0 {
		return locatedLine{}, false
	}
	lineIDs := make(map[string]bool, len(field.Candidates[0].LineIDs))
	for _, id := range field.Candidates[0].LineIDs {
		lineIDs[id] = true
	}
	for _, line := range lines {
		if lineIDs[line.line.ID] {
			if _, ok := parseDate(line.line.TextRaw); ok {
				return line, true
			}
		}
	}
	anchorID := field.Candidates[0].LineIDs[0]
	for _, line := range lines {
		if line.line.ID == anchorID {
			return line, true
		}
	}
	return locatedLine{}, false
}

func layoutCandidate(input Observations, field string, lines []locatedLine, rule, context, issue string) FieldResult {
	drafts := make([]candidateDraft, 0, len(lines))
	if len(lines) == 0 {
		return emptyField()
	}
	value := cleanValue(joinRaw(lines))
	if value == "" {
		return emptyField()
	}
	drafts = append(drafts, candidateDraft{value: value, raw: joinRaw(lines), lines: lines, rules: []string{rule}, context: context})
	result := resolveDrafts(input, field, drafts, true, false)
	result.Issues = appendUnique(result.Issues, issue)
	return result
}

func extractLayoutGender(input Observations, lines []locatedLine, birthDate FieldResult) FieldResult {
	anchor, ok := evidenceAnchor(lines, birthDate)
	if !ok {
		return emptyField()
	}
	var drafts []candidateDraft
	for _, line := range lines {
		if line.pass.ID != anchor.pass.ID || pageOf(input, line.y) != "lower" ||
			!lineOverlapsLayoutZone(line, input.Document.Bounds[0]+_layoutRightOfPhoto*docWidth(input), input.Document.Bounds[2], _layoutGenderZoneOverlap) ||
			abs(line.y-anchor.y) > _layoutNearDateY*docHeight(input) || isMRZLike(input, line) ||
			hasAnyLabel(line.line.TextRaw, allLabelTerms()) {
			continue
		}
		canonical, ok := canonicalLayoutGender(line.line.TextRaw)
		if !ok {
			continue
		}
		drafts = append(drafts, candidateDraft{
			value: canonical, raw: line.line.TextRaw, lines: []locatedLine{line},
			rules:   []string{"lower_page_gender_role_fallback"},
			context: "layout_fallback:lower_page_gender_near_birth_date",
		})
	}
	result := resolveDrafts(input, "gender", drafts, true, false)
	if len(result.Candidates) > 0 {
		result.Issues = appendUnique(result.Issues, "gender_layout_fallback_requires_manual_review")
	}
	return result
}

func canonicalLayoutGender(raw string) (string, bool) {
	// Accept only whole-word Russian gender tokens. Single-letter MRZ values are
	// intentionally excluded from this unlabeled layout fallback.
	switch canonicalLabel(raw) {
	case "МУЖ", "МУЖСКОЙ":
		return "M", true
	case "ЖЕН", "ЖЕНСКИЙ":
		return "F", true
	default:
		return "", false
	}
}

func lineOverlapsLayoutZone(line locatedLine, zoneMinX, zoneMaxX, minimumFraction float64) bool {
	minX, maxX := math.Inf(1), math.Inf(-1)
	for _, point := range line.line.Quad {
		minX = math.Min(minX, point[0])
		maxX = math.Max(maxX, point[0])
	}
	width := maxX - minX
	if width <= 0 {
		return false
	}
	overlap := math.Max(0, math.Min(maxX, zoneMaxX)-math.Max(minX, zoneMinX))
	return overlap >= minimumFraction*width
}

func extractLayoutDepartmentCode(input Observations, lines []locatedLine, issueDate FieldResult) FieldResult {
	anchor, ok := evidenceAnchor(lines, issueDate)
	if !ok || pageOf(input, anchor.y) != "upper" {
		return emptyField()
	}
	var drafts []candidateDraft
	for _, line := range lines {
		if line.pass.ID != anchor.pass.ID || pageOf(input, line.y) != "upper" ||
			line.x < anchor.x || abs(line.y-anchor.y) > _layoutNearDateY*docHeight(input) ||
			isMRZLike(input, line) || hasAnyLabel(line.line.TextRaw, allLabelTerms()) {
			continue
		}
		digits, numeric := numericToken(line.line.TextRaw)
		if !numeric || (!digitRun6.MatchString(digits) && !deptDash.MatchString(strings.TrimSpace(line.line.TextRaw))) || !horizontalNumericLine(line) {
			continue
		}
		value := digits
		if deptDash.MatchString(strings.TrimSpace(line.line.TextRaw)) {
			value = strings.ReplaceAll(strings.TrimSpace(line.line.TextRaw), " ", "")
		}
		drafts = append(drafts, candidateDraft{
			value: value, raw: line.line.TextRaw, lines: []locatedLine{line},
			rules:   []string{"upper_page_department_code_near_issue_date_fallback"},
			context: "layout_fallback:upper_page_right_of_issue_date",
		})
	}
	result := resolveDrafts(input, "department_code", drafts, true, false)
	if len(result.Candidates) > 0 {
		result.Issues = appendUnique(result.Issues, "department_code_layout_fallback_requires_manual_review")
	}
	return result
}

var authorityKeywords = []string{"МВД", "УФМС", "ФМС", "УВД", "ОВД", "ОТДЕЛ", "УПРАВЛЕНИ"}

func extractLayoutIssuer(input Observations, lines []locatedLine, issueDate FieldResult) FieldResult {
	dateLine, ok := evidenceAnchor(lines, issueDate)
	if !ok || pageOf(input, dateLine.y) != "upper" {
		return emptyField()
	}
	var drafts []candidateDraft
	for _, anchor := range lines {
		if anchor.pass.ID != dateLine.pass.ID || pageOf(input, anchor.y) != "upper" || anchor.y >= dateLine.y ||
			!hasAnyLabel(anchor.line.TextRaw, authorityKeywords) || isExcludedLayoutText(input, anchor) {
			continue
		}
		block := issuerAuthorityBlock(input, lines, anchor, dateLine)
		if len(block) == 0 {
			continue
		}
		value := cleanValue(joinRaw(block))
		if value != "" {
			drafts = append(drafts, candidateDraft{
				value: value, raw: joinRaw(block), lines: block,
				rules:   []string{"upper_page_authority_anchor_block_fallback"},
				context: "layout_fallback:issuer_authority_anchor_before_issue_date",
			})
		}
	}
	result := resolveDrafts(input, "issuer", drafts, true, false)
	if len(result.Candidates) > 0 {
		result.Issues = appendUnique(result.Issues, "issuer_layout_fallback_requires_manual_review")
	}
	return result
}

func issuerAuthorityBlock(input Observations, lines []locatedLine, anchor, issueDate locatedLine) []locatedLine {
	var samePass []locatedLine
	for _, line := range lines {
		if line.pass.ID == anchor.pass.ID && line.y < issueDate.y && pageOf(input, line.y) == "upper" {
			samePass = append(samePass, line)
		}
	}
	rows := groupRows(samePass, 0.018*docHeight(input))
	anchorIndex := -1
	for index, row := range rows {
		for _, line := range row {
			if line.line.ID == anchor.line.ID {
				anchorIndex = index
				break
			}
		}
	}
	if anchorIndex < 0 {
		return nil
	}
	anchorRow := issuerLaneLines(anchor, rows[anchorIndex], input)
	if len(anchorRow) == 0 {
		return nil
	}
	selectedByRow := map[int][]locatedLine{anchorIndex: anchorRow}
	nearbyRows := scanIssuerRows(input, rows, anchorIndex, anchor, issueDate)
	sort.SliceStable(nearbyRows, func(i, j int) bool {
		return abs(rowCenterY(nearbyRows[i].lines)-anchor.y) < abs(rowCenterY(nearbyRows[j].lines)-anchor.y)
	})
	for _, row := range nearbyRows {
		if len(selectedByRow) == 4 {
			break
		}
		selectedByRow[row.index] = row.lines
	}
	var selected []locatedLine
	for index := range rows {
		selected = append(selected, selectedByRow[index]...)
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].y == selected[j].y {
			return selected[i].x < selected[j].x
		}
		return selected[i].y < selected[j].y
	})
	return selected
}

func selectedRows(selected []locatedLine, rows [][]locatedLine) [][]locatedLine {
	ids := make(map[string]bool, len(selected))
	for _, line := range selected {
		ids[line.line.ID] = true
	}
	var out [][]locatedLine
	for _, row := range rows {
		if len(row) > 0 && ids[row[0].line.ID] {
			out = append(out, row)
		}
	}
	return out
}

type issuerRow struct {
	index int
	lines []locatedLine
}

func scanIssuerRows(
	input Observations,
	rows [][]locatedLine,
	anchorIndex int,
	anchor, issueDate locatedLine,
) []issuerRow {
	var accepted []issuerRow
	lastAcceptedY := anchor.y
	for index := anchorIndex + 1; index < len(rows); index++ {
		row := rows[index]
		y := rowCenterY(row)
		if abs(y-anchor.y) > _layoutMaxBlockHeight*docHeight(input) || y >= issueDate.y {
			break
		}
		boundary := false
		for _, line := range row {
			if isExcludedLayoutText(input, line) {
				boundary = true
				break
			}
		}
		if boundary {
			break
		}
		aligned := issuerLaneLines(anchor, row, input)
		if len(aligned) == 0 {
			if abs(y-lastAcceptedY) > _layoutMaxRowGap*docHeight(input) {
				break
			}
			continue
		}
		alignedY := rowCenterY(aligned)
		if abs(alignedY-anchor.y) > _layoutMaxBlockHeight*docHeight(input) ||
			abs(alignedY-lastAcceptedY) > _layoutMaxRowGap*docHeight(input) {
			break
		}
		accepted = append(accepted, issuerRow{index: index, lines: aligned})
		lastAcceptedY = alignedY
	}
	return accepted
}

func issuerLaneLines(anchor locatedLine, row []locatedLine, input Observations) []locatedLine {
	var aligned []locatedLine
	for _, line := range row {
		if issuerLineSharesLane(anchor, line, input) {
			aligned = append(aligned, line)
		}
	}
	return aligned
}

func issuerLineSharesLane(anchor, line locatedLine, input Observations) bool {
	anchorMinX, anchorMaxX, anchorHeight := quadBounds(anchor.line.Quad)
	lineMinX, lineMaxX, lineHeight := quadBounds(line.line.Quad)
	anchorWidth, lineWidth := anchorMaxX-anchorMinX, lineMaxX-lineMinX
	if anchorWidth <= 0 || lineWidth <= 0 {
		return false
	}
	overlap := math.Max(0, math.Min(anchorMaxX, lineMaxX)-math.Max(anchorMinX, lineMinX))
	if overlap < 0.25*math.Min(anchorWidth, lineWidth) {
		return false
	}
	centerTolerance := 0.10 * docWidth(input)
	if heightTolerance := 2 * math.Max(anchorHeight, lineHeight); heightTolerance > centerTolerance {
		centerTolerance = heightTolerance
	}
	leftTolerance := 0.06 * docWidth(input)
	if heightTolerance := math.Max(anchorHeight, lineHeight); heightTolerance > leftTolerance {
		leftTolerance = heightTolerance
	}
	anchorCenter, lineCenter := (anchorMinX+anchorMaxX)/2, (lineMinX+lineMaxX)/2
	return abs(anchorCenter-lineCenter) <= centerTolerance || abs(anchorMinX-lineMinX) <= leftTolerance
}

func quadBounds(quad Quad) (minX, maxX, height float64) {
	minX, maxX = math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, point := range quad {
		minX = math.Min(minX, point[0])
		maxX = math.Max(maxX, point[0])
		minY = math.Min(minY, point[1])
		maxY = math.Max(maxY, point[1])
	}
	return minX, maxX, maxY - minY
}

func extractLayoutBirthPlace(input Observations, lines []locatedLine, birthDate FieldResult) FieldResult {
	dateLine, ok := evidenceAnchor(lines, birthDate)
	if !ok || pageOf(input, dateLine.y) != "lower" {
		return emptyField()
	}
	var samePass []locatedLine
	for _, line := range lines {
		if line.pass.ID != dateLine.pass.ID || pageOf(input, line.y) != "lower" {
			continue
		}
		samePass = append(samePass, line)
	}
	rows := groupRows(samePass, 0.018*docHeight(input))
	anchorIndex := -1
	for index, row := range rows {
		for _, line := range row {
			if line.line.ID == dateLine.line.ID {
				anchorIndex = index
			}
		}
	}
	if anchorIndex < 0 {
		return emptyField()
	}
	var selected []locatedLine
	lastY := dateLine.y
	for _, row := range rows[anchorIndex+1:] {
		y := rowCenterY(row)
		if y-dateLine.y > _layoutMaxBlockHeight*docHeight(input) || y-lastY > _layoutMaxRowGap*docHeight(input) {
			break
		}
		if len(selectedRows(selected, rows)) >= 4 || rowCenterX(row) < input.Document.Bounds[0]+_layoutRightOfPhoto*docWidth(input) {
			break
		}
		eligible := true
		for _, line := range row {
			if isExcludedLayoutText(input, line) {
				eligible = false
				break
			}
		}
		if !eligible {
			break
		}
		selected = append(selected, row...)
		lastY = y
	}
	return layoutCandidate(input, "birth_place", selected, "lower_page_birth_place_after_birth_date_fallback", "layout_fallback:lower_page_after_birth_date", "birth_place_layout_fallback_requires_manual_review")
}

func isExcludedLayoutText(input Observations, line locatedLine) bool {
	if !hasLetters(line.line.TextRaw) || hasAnyLabel(line.line.TextRaw, allLabelTerms()) || isMRZLike(input, line) {
		return true
	}
	if _, ok := parseDate(line.line.TextRaw); ok {
		return true
	}
	if digits, numeric := numericToken(line.line.TextRaw); numeric && digits != "" {
		return true
	}
	return false
}

func isMRZLike(input Observations, line locatedLine) bool {
	if strings.Contains(line.line.TextRaw, "<<") {
		return true
	}
	x0, y0, x1, y1 := input.Document.Bounds[0], input.Document.Bounds[1], input.Document.Bounds[2], input.Document.Bounds[3]
	width := (distance(line.line.Quad[0], line.line.Quad[1]) + distance(line.line.Quad[3], line.line.Quad[2])) / 2
	return line.y >= y0+_layoutMRZStart*(y1-y0) && width >= _layoutWideLineFraction*(x1-x0)
}

func horizontalNumericLine(line locatedLine) bool {
	width := (distance(line.line.Quad[0], line.line.Quad[1]) + distance(line.line.Quad[3], line.line.Quad[2])) / 2
	height := (distance(line.line.Quad[0], line.line.Quad[3]) + distance(line.line.Quad[1], line.line.Quad[2])) / 2
	return width > 0 && height > 0 && width >= 1.2*height
}

func distance(a, b Point) float64 {
	return math.Hypot(a[0]-b[0], a[1]-b[1])
}

func rowCenterY(row []locatedLine) float64 {
	var y float64
	for _, line := range row {
		y += line.y
	}
	return y / float64(len(row))
}

func rowCenterX(row []locatedLine) float64 {
	var x float64
	for _, line := range row {
		x += line.x
	}
	return x / float64(len(row))
}

func extractUnlabeledDate(input Observations, lines []locatedLine, issue bool) FieldResult {
	var drafts []candidateDraft
	for _, line := range lines {
		if value, ok := parseDate(line.line.TextRaw); ok {
			page := pageOf(input, line.y)
			if (issue && page == "upper") || (!issue && page == "lower") {
				drafts = append(drafts, candidateDraft{value: value, raw: line.line.TextRaw, lines: []locatedLine{line}, rules: []string{"page_region_date_role_fallback"}, context: "layout_fallback:" + page + "_page_date_role"})
			}
		}
	}
	field := "birth_date"
	if issue {
		field = "issue_date"
	}
	return resolveDrafts(input, field, drafts, true, false)
}

func extractSerialNumber(input Observations, lines []locatedLine) (FieldResult, FieldResult) {
	var labeledSeries, labeledNumber []candidateDraft
	for _, anchor := range lines {
		for _, field := range []string{"series", "number"} {
			terms := labelTerms[field]
			if !hasAnyLabel(anchor.line.TextRaw, terms) {
				continue
			}
			valuePart := stripLabel(anchor.line.TextRaw, terms)
			labeledSeries, labeledNumber = appendSerialFieldDrafts(
				labeledSeries, labeledNumber, valuePart, anchor.line.TextRaw, []locatedLine{anchor},
				"explicit_label", field,
			)
			if valuePart == "" {
				for _, near := range nearbyValueLines(input, lines, anchor) {
					labeledSeries, labeledNumber = appendSerialFieldDrafts(
						labeledSeries, labeledNumber, near.line.TextRaw, anchor.line.TextRaw+"\n"+near.line.TextRaw,
						[]locatedLine{anchor, near}, "explicit_label_spatial_association", field,
					)
				}
			}
		}
	}
	var series, number []candidateDraft
	for _, line := range lines {
		if !serialLayoutLine(input, line) {
			continue
		}
		digits, numeric := numericToken(line.line.TextRaw)
		if numeric && digitRun10.MatchString(digits) {
			series, number = appendSerialSplit(series, number, digits[:4], digits[4:], line.line.TextRaw, []locatedLine{line}, "split_exact_4_plus_6_digits", "layout_fallback:right_margin_ten_digit_run")
		} else if numeric && digitRun6.MatchString(digits) && isVerticalSerialContext(line) {
			number = append(number, candidateDraft{
				value: digits, raw: line.line.TextRaw, lines: []locatedLine{line},
				rules:   []string{"single_six_digit_right_margin_number_fallback"},
				context: "layout_fallback:right_margin_single_six_digit_number",
			})
		}
	}
	// Adjacent rows are joined only within one pass and the right-margin serial
	// region. Exact lengths are required; leading zeroes are retained.
	for _, a := range lines {
		ad, aNumeric := numericToken(a.line.TextRaw)
		if !serialLayoutLine(input, a) || !aNumeric || !digitRun4.MatchString(ad) {
			continue
		}
		for _, b := range lines {
			bd, bNumeric := numericToken(b.line.TextRaw)
			if !serialLayoutLine(input, b) || !bNumeric || b.pass.ID != a.pass.ID || !digitRun6.MatchString(bd) || b.y < a.y || b.y-a.y > 0.10*docHeight(input) || abs(a.x-b.x) > 0.18*docWidth(input) {
				continue
			}
			both := []locatedLine{a, b}
			series, number = appendSerialSplit(series, number, ad, bd, joinRaw(both), both, "join_adjacent_4_digit_series_and_6_digit_number", "layout_fallback:right_margin_four_plus_six")
		}
	}
	for _, a := range lines {
		ad, aNumeric := numericToken(a.line.TextRaw)
		if !serialLayoutLine(input, a) || !aNumeric || !digitRun2.MatchString(ad) {
			continue
		}
		for _, b := range lines {
			bd, bNumeric := numericToken(b.line.TextRaw)
			if !serialLayoutLine(input, b) || !bNumeric || b.line.ID == a.line.ID || b.pass.ID != a.pass.ID || !digitRun2.MatchString(bd) || b.y <= a.y || b.y-a.y > 0.10*docHeight(input) || abs(a.x-b.x) > 0.18*docWidth(input) {
				continue
			}
			for _, c := range lines {
				cd, cNumeric := numericToken(c.line.TextRaw)
				if !serialLayoutLine(input, c) || !cNumeric || c.line.ID == a.line.ID || c.line.ID == b.line.ID || c.pass.ID != a.pass.ID || !digitRun6.MatchString(cd) || c.y <= b.y || c.y-b.y > 0.10*docHeight(input) || abs(b.x-c.x) > 0.18*docWidth(input) {
					continue
				}
				parts := []locatedLine{a, b, c}
				series, number = appendSerialSplit(series, number, ad+bd, cd, joinRaw(parts), parts, "join_adjacent_two_two_six_digit_records", "layout_fallback:right_margin_two_two_six")
			}
		}
	}
	if len(labeledSeries) > 0 {
		series = labeledSeries
	}
	if len(labeledNumber) > 0 {
		number = labeledNumber
	}
	seriesReview, numberReview := len(labeledSeries) == 0, len(labeledNumber) == 0
	return resolveDrafts(input, "series", series, seriesReview, false), resolveDrafts(input, "number", number, numberReview, false)
}

var digitRun2 = regexp.MustCompile(`^\d{2}$`)

func serialLayoutLine(input Observations, line locatedLine) bool {
	return line.x >= input.Document.Bounds[0]+_layoutRightMargin*docWidth(input) &&
		!isMRZLike(input, line) && (horizontalNumericLine(line) || verticalMarginNumericLine(line))
}

func verticalMarginNumericLine(line locatedLine) bool {
	if line.pass.Kind != "original" && line.pass.Kind != "right_margin_cw" && line.pass.Kind != "right_margin_ccw" {
		return false
	}
	width := (distance(line.line.Quad[0], line.line.Quad[1]) + distance(line.line.Quad[3], line.line.Quad[2])) / 2
	height := (distance(line.line.Quad[0], line.line.Quad[3]) + distance(line.line.Quad[1], line.line.Quad[2])) / 2
	return height > 0 && width > 0 && height >= 1.2*width
}

func isVerticalSerialContext(line locatedLine) bool {
	if line.pass.Kind == "right_margin_cw" || line.pass.Kind == "right_margin_ccw" {
		return true
	}
	return verticalMarginNumericLine(line)
}

func appendSerialFieldDrafts(series, number []candidateDraft, raw, sourceRaw string, lines []locatedLine, rule, field string) ([]candidateDraft, []candidateDraft) {
	digits, numeric := numericToken(raw)
	if !numeric {
		return series, number
	}
	if digitRun10.MatchString(digits) {
		return appendSerialSplit(series, number, digits[:4], digits[4:], sourceRaw, lines, "split_explicit_labeled_ten_digit_run", "explicit_label:serial_number_split")
	}
	context := "explicit_label:" + field
	if field == "series" && digitRun4.MatchString(digits) {
		series = append(series, candidateDraft{value: digits, raw: sourceRaw, lines: lines, rules: []string{rule + "_series_value"}, context: context})
	}
	if field == "number" && digitRun6.MatchString(digits) {
		number = append(number, candidateDraft{value: digits, raw: sourceRaw, lines: lines, rules: []string{rule + "_number_value"}, context: context})
	}
	return series, number
}

func appendSerialSplit(series, number []candidateDraft, seriesValue, numberValue, raw string, lines []locatedLine, rule, context string) ([]candidateDraft, []candidateDraft) {
	series = append(series, candidateDraft{value: seriesValue, raw: raw, lines: lines, rules: []string{rule}, context: context})
	number = append(number, candidateDraft{value: numberValue, raw: raw, lines: lines, rules: []string{rule}, context: context})
	return series, number
}

func resolveDrafts(input Observations, field string, drafts []candidateDraft, forceUncertain bool, normalizeNames bool) FieldResult {
	result := emptyField()
	if len(drafts) == 0 {
		return result
	}
	byValue := make(map[string]Candidate)
	for _, draft := range drafts {
		if draft.value == "" {
			continue
		}
		rules := append([]string(nil), draft.rules...)
		value := draft.value
		review := false
		normalizationProposed := false
		if field == "surname" || field == "given_name" || field == "patronymic" {
			var normalizedRules []string
			var needsReview bool
			value, normalizedRules, needsReview = normalizeName(value, normalizeNames)
			rules = append(rules, normalizedRules...)
			review = needsReview
			normalizationProposed = normalizeNames && len(normalizedRules) > 0
		}
		if field == "gender" {
			rawValue := value
			var ok bool
			value, ok = canonicalGender(value)
			if !ok {
				result.Issues = append(result.Issues, "gender_value_not_unambiguous")
				continue
			}
			if rawValue != value {
				rules = append(rules, "gender_label_value_to_canonical_code")
			}
		}
		if field == "birth_date" || field == "issue_date" {
			parsed, ok := parseDate(value)
			if !ok {
				result.Issues = append(result.Issues, "date_invalid_calendar_or_format")
				continue
			}
			value = parsed
			if draft.value != parsed {
				rules = append(rules, "date_dd_mm_yyyy_to_iso8601")
			}
		}
		if field == "department_code" {
			rawValue := value
			value = canonicalDepartment(value)
			if !deptDash.MatchString(value) {
				result.Issues = append(result.Issues, "department_code_invalid_format")
				continue
			}
			if rawValue != value {
				rules = append(rules, "six_digit_department_code_to_3_3")
			}
		}
		candidate := makeCandidate(value, draft.raw, draft.lines, rules, draft.context)
		if existing, ok := byValue[value]; ok {
			byValue[value] = mergeCandidate(existing, candidate)
		} else {
			byValue[value] = candidate
		}
		if review {
			if normalizationProposed {
				result.Issues = appendUnique(result.Issues, "normalization_proposal_requires_manual_review")
			} else {
				result.Issues = appendUnique(result.Issues, "name_script_requires_manual_review")
			}
		}
		if input.Document.Quality == "fallback" || input.Document.Method == "full_fallback" {
			forceUncertain = true
			result.Issues = appendUnique(result.Issues, "document_bounds_fallback_requires_review")
		}
		if input.Document.Method == "ocr_bbox_envelope" {
			forceUncertain = true
			result.Issues = appendUnique(result.Issues, "document_bounds_ocr_bbox_envelope_provisional_requires_review")
		}
	}
	candidates := make([]Candidate, 0, len(byValue))
	for _, candidate := range byValue {
		candidates = append(candidates, candidate)
	}
	return resolveCandidateSet(input, field, candidates, result.Issues, forceUncertain)
}

// resolveCandidateSet is the common final candidate deduplication/conflict
// resolver for OCR drafts and validated external field evidence.
func resolveCandidateSet(input Observations, field string, candidates []Candidate, issues []string, forceUncertain bool) FieldResult {
	result := emptyField()
	result.Issues = append([]string(nil), issues...)
	byValue := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.Value == "" {
			continue
		}
		if existing, ok := byValue[candidate.Value]; ok {
			byValue[candidate.Value] = mergeCandidate(existing, candidate)
		} else {
			byValue[candidate.Value] = candidate
		}
	}
	for _, candidate := range byValue {
		result.Candidates = append(result.Candidates, candidate)
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].Value == result.Candidates[j].Value {
			return strings.Join(result.Candidates[i].LineIDs, ",") < strings.Join(result.Candidates[j].LineIDs, ",")
		}
		return result.Candidates[i].Value < result.Candidates[j].Value
	})
	// Candidate iteration above came from a map. Aggregate evidence only after
	// sorting so conflict metadata and downstream baseline hashes are stable.
	for _, candidate := range result.Candidates {
		result.Evidence.LineIDs = uniqueStrings(append(result.Evidence.LineIDs, candidate.LineIDs...))
		result.Evidence.PassIDs = uniqueStrings(append(result.Evidence.PassIDs, candidate.PassIDs...))
	}
	if len(result.Candidates) == 0 {
		return result
	}
	if len(result.Candidates) > 1 {
		result.State = "uncertain"
		result.Issues = appendUnique(result.Issues, "conflicting_field_candidates")
		return result
	}
	chosen := result.Candidates[0]
	result.Raw = chosen.Raw
	result.Rules = append([]string(nil), chosen.Rules...)
	result.Evidence = Evidence{LineIDs: append([]string(nil), chosen.LineIDs...), PassIDs: append([]string(nil), chosen.PassIDs...)}
	result.Context = chosen.Context
	if hasIssue(result.Issues, "date_invalid_calendar_or_format") || hasIssue(result.Issues, "gender_value_not_unambiguous") || hasIssue(result.Issues, "department_code_invalid_format") {
		result.State = "uncertain"
		return result
	}
	v := chosen.Value
	result.Value = &v
	if forceUncertain || hasIssue(result.Issues, "normalization_proposal_requires_manual_review") || hasIssue(result.Issues, "name_script_requires_manual_review") {
		result.State = "uncertain"
	} else {
		result.State = "recognized"
	}
	return result
}

func makeCandidate(value, raw string, lines []locatedLine, rules []string, context string) Candidate {
	lineIDs, passIDs := make([]string, 0, len(lines)), make([]string, 0, len(lines))
	seenLines, seenPasses := make(map[string]bool), make(map[string]bool)
	for _, line := range lines {
		if !seenLines[line.line.ID] {
			lineIDs = append(lineIDs, line.line.ID)
			seenLines[line.line.ID] = true
		}
		if !seenPasses[line.pass.ID] {
			passIDs = append(passIDs, line.pass.ID)
			seenPasses[line.pass.ID] = true
		}
	}
	return Candidate{Value: value, Raw: raw, RawVariants: []string{raw}, Rules: uniqueStrings(rules), LineIDs: lineIDs, PassIDs: passIDs, Context: context}
}

func mergeCandidate(a, b Candidate) Candidate {
	a.TemplateEvidence = mergeTemplateProvenance(a.TemplateEvidence, b.TemplateEvidence)
	a.VLEvidence = mergeVLProvenance(a.VLEvidence, b.VLEvidence)
	a.LineIDs = uniqueStrings(append(a.LineIDs, b.LineIDs...))
	a.PassIDs = uniqueStrings(append(a.PassIDs, b.PassIDs...))
	a.RawVariants = uniqueStrings(append(a.RawVariants, b.RawVariants...))
	a.Rules = uniqueStrings(append(a.Rules, b.Rules...))
	for _, source := range b.FieldEvidence {
		found := false
		for _, existing := range a.FieldEvidence {
			if existing.ImageSHA256 == source.ImageSHA256 && existing.ROIBounds == source.ROIBounds && existing.Reader == source.Reader && existing.ReaderModelSHA256 == source.ReaderModelSHA256 {
				found = true
				break
			}
		}
		if !found {
			a.FieldEvidence = append(a.FieldEvidence, source)
		}
	}
	if a.Context != b.Context {
		a.Context = "multiple_evidence_contexts"
	}
	return a
}

func nearbyValueLines(input Observations, lines []locatedLine, anchor locatedLine) []locatedLine {
	var candidates []locatedLine
	for _, line := range lines {
		if line.line.ID == anchor.line.ID || line.pass.ID != anchor.pass.ID || hasAnyLabel(line.line.TextRaw, allLabelTerms()) {
			continue
		}
		dx, dy := line.x-anchor.x, line.y-anchor.y
		sameRow := abs(dy) <= 0.018*docHeight(input) && dx > 0 && dx <= 0.62*docWidth(input)
		below := dy > 0 && dy <= 0.045*docHeight(input) && abs(dx) <= 0.28*docWidth(input)
		if sameRow || below {
			candidates = append(candidates, line)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		di := abs(candidates[i].y-anchor.y)*2 + abs(candidates[i].x-anchor.x)
		dj := abs(candidates[j].y-anchor.y)*2 + abs(candidates[j].x-anchor.x)
		return di < dj
	})
	if len(candidates) > 3 {
		return candidates[:3]
	}
	return candidates
}

func canonicalFieldValue(field, raw string) string {
	value := cleanValue(raw)
	switch field {
	case "birth_date", "issue_date":
		return value
	case "gender":
		canonical, ok := canonicalGender(value)
		if ok {
			return canonical
		}
	}
	return value
}

func parseDate(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	for _, pattern := range []*regexp.Regexp{dateDot, dateDash} {
		matches := pattern.FindStringSubmatch(value)
		if len(matches) == 4 {
			parsed, err := time.Parse("02-01-2006", matches[1]+"-"+matches[2]+"-"+matches[3])
			if err != nil {
				return "", false
			}
			return parsed.Format("2006-01-02"), true
		}
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
	if deptDash.MatchString(value) {
		return value
	}
	digits, numeric := numericToken(value)
	if numeric && digitRun6.MatchString(digits) {
		return digits[:3] + "-" + digits[3:]
	}
	return value
}

func hasAnyLabel(raw string, terms []string) bool {
	canonical := canonicalLabel(raw)
	for _, term := range terms {
		if strings.Contains(canonical, term) {
			return true
		}
	}
	return false
}

func stripLabel(raw string, terms []string) string {
	text := strings.TrimSpace(raw)
	if i := strings.IndexAny(text, ":："); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	upper := strings.ToUpper(text)
	for _, term := range terms {
		idx := strings.Index(upper, term)
		if idx >= 0 {
			return strings.TrimSpace(text[idx+len(term):])
		}
	}
	return ""
}

func canonicalLabel(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if replacement, ok := labelLookalike[r]; ok {
			r = replacement
		}
		if (r >= 'А' && r <= 'Я') || r == 'Ё' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// labelLookalike is used only to match OCR labels; source and proposed values
// are never transliterated by this table.
var labelLookalike = map[rune]rune{
	'A': 'А', 'B': 'В', 'C': 'С', 'E': 'Е', 'H': 'Н', 'K': 'К',
	'M': 'М', 'O': 'О', 'P': 'Р', 'T': 'Т', 'X': 'Х', 'Y': 'У',
}

func allLabelTerms() []string {
	var all []string
	for _, terms := range labelTerms {
		all = append(all, terms...)
	}
	return all
}

func cleanValue(s string) string {
	s = strings.TrimSpace(strings.Trim(s, ":;,. 	\r\n"))
	return strings.Join(strings.Fields(s), " ")
}

func usableText(s string) bool { return strings.TrimSpace(s) != "" }

func hasLetters(s string) bool {
	for _, r := range s {
		if (r >= 'А' && r <= 'Я') || (r >= 'а' && r <= 'я') || r == 'Ё' || r == 'ё' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return true
		}
	}
	return false
}

func nameLike(s string) bool {
	if !hasLetters(s) || len([]rune(s)) > 60 || strings.ContainsAny(s, "0123456789") {
		return false
	}
	return true
}

func numericToken(s string) (string, bool) {
	var digits strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == ' ' || r == '\t' || r == '-' || r == '‐' || r == '‑':
			// Common visual separators are permitted; other characters are not.
		default:
			return "", false
		}
	}
	return digits.String(), true
}

func joinRaw(lines []locatedLine) string {
	ordered := append([]locatedLine(nil), lines...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].y == ordered[j].y {
			return ordered[i].x < ordered[j].x
		}
		return ordered[i].y < ordered[j].y
	})
	parts := make([]string, 0, len(ordered))
	for _, line := range ordered {
		parts = append(parts, line.line.TextRaw)
	}
	return strings.Join(parts, "\n")
}

func groupRows(lines []locatedLine, tolerance float64) [][]locatedLine {
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].y == lines[j].y {
			return lines[i].x < lines[j].x
		}
		return lines[i].y < lines[j].y
	})
	var rows [][]locatedLine
	for _, line := range lines {
		if len(rows) == 0 || abs(line.y-rows[len(rows)-1][0].y) > tolerance {
			rows = append(rows, []locatedLine{line})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], line)
		}
	}
	for _, row := range rows {
		sort.Slice(row, func(i, j int) bool { return row[i].x < row[j].x })
	}
	return rows
}

func pageOf(input Observations, y float64) string {
	seam := pageSeam(input)
	if y < seam {
		return "upper"
	}
	return "lower"
}

func pageSeam(input Observations) float64 {
	if input.Document.SeamY != nil {
		return *input.Document.SeamY
	}
	return input.Document.Bounds[1] + 0.50*docHeight(input)
}

func docWidth(input Observations) float64 { return input.Document.Bounds[2] - input.Document.Bounds[0] }
func docHeight(input Observations) float64 {
	return input.Document.Bounds[3] - input.Document.Bounds[1]
}

func center(quad Quad) (float64, float64) {
	x, y := 0.0, 0.0
	for _, p := range quad {
		x += p[0]
		y += p[1]
	}
	return x / 4, y / 4
}

func abs(n float64) float64 {
	if n < 0 {
		return -n
	}
	return n
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result
}

func hasIssue(values []string, issue string) bool {
	for _, value := range values {
		if value == issue {
			return true
		}
	}
	return false
}
