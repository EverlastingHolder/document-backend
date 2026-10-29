package passport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const VLFieldPlanSchema = "passport-vl-field-plan/v1"

var vlPlanFields = []string{"surname", "given_name", "patronymic", "gender", "birth_place", "issuer"}

var vlDateToken = regexp.MustCompile(`\d{4}\s*-\s*\d{2}\s*-\s*\d{2}|\d{2}\s*[./-]\s*\d{2}\s*[./-]\s*\d{4}|\d{2}\s+\d{2}\s+\d{4}`)

var vlPassportHeaderTerms = []string{"ПОДПИСЬ", "ЛИЧНАЯПОДПИСЬ", "ДЛЯОТМЕТОК", "ЛИЧНЫЙКОД"}

type VLFieldPlan struct {
	SchemaVersion  string           `json:"schema_version"`
	Image          ImageObservation `json:"image"`
	BaselineSHA256 string           `json:"baseline_sha256"`
	PlanSHA256     string           `json:"plan_sha256,omitempty"`
	Regions        []VLFieldRegion  `json:"regions"`
	SkippedFields  []VLFieldSkip    `json:"skipped_fields"`
}

type VLFieldRegion struct {
	ID            string   `json:"id"`
	FieldID       string   `json:"field_id"`
	Recipe        string   `json:"recipe"`
	Crop          Rect     `json:"crop"`
	AnchorLineIDs []string `json:"anchor_line_ids"`
}

type VLFieldSkip struct {
	FieldID string `json:"field_id"`
	Reason  string `json:"reason"`
}

type vlPlanLine struct {
	line OCRLine
	pass OCRPass
	box  Rect
	x    float64
	y    float64
	ids  []string
}

type vlCoherentNameBlock struct {
	rows   [][]vlPlanLine
	dobIDs []string
	dobBox Rect
	hasDOB bool
}

func BuildVLFieldPlan(input Observations, baseline Extraction) (VLFieldPlan, error) {
	if err := input.Validate(); err != nil {
		return VLFieldPlan{}, err
	}
	if baseline.SchemaVersion != ExtractionSchema || baseline.ImageID != input.Image.ID || baseline.Fields == nil {
		return VLFieldPlan{}, errors.New("invalid baseline extraction for VL plan")
	}
	baselineBytes, err := json.Marshal(baseline)
	if err != nil {
		return VLFieldPlan{}, err
	}
	hash := sha256.Sum256(baselineBytes)
	plan := VLFieldPlan{
		SchemaVersion:  VLFieldPlanSchema,
		Image:          input.Image,
		BaselineSHA256: hex.EncodeToString(hash[:]),
		Regions:        []VLFieldRegion{},
		SkippedFields:  []VLFieldSkip{},
	}
	lines := vlPlanLineMaps(input)
	templateRegions := validBaselineTemplateRegions(input, baseline)
	nameBlock, nameReason := vlFindCoherentNameBlock(input, baseline, lines)
	for _, field := range vlPlanFields {
		if region, ok := templateRegions[field]; ok {
			plan.Regions = append(plan.Regions, region)
			continue
		}
		if vlNameField(field) {
			region, reason := vlNameFieldRegion(input, field, nameBlock)
			if reason == "" {
				plan.Regions = append(plan.Regions, region)
				continue
			}
			if nameReason != "" {
				reason = nameReason
			}
			plan.SkippedFields = append(plan.SkippedFields, VLFieldSkip{FieldID: field, Reason: reason})
			continue
		}
		region, reason := vlPlanBaselineField(input, baseline, field, lines, nameBlock)
		if reason == "" {
			plan.Regions = append(plan.Regions, region)
			continue
		}
		plan.SkippedFields = append(plan.SkippedFields, VLFieldSkip{FieldID: field, Reason: reason})
	}
	plan.PlanSHA256, err = VLFieldPlanSHA256(plan)
	if err != nil {
		return VLFieldPlan{}, err
	}
	return plan, nil
}

func VLFieldPlanSHA256(plan VLFieldPlan) (string, error) {
	plan.PlanSHA256 = ""
	value, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:]), nil
}

func vlPlanLineMaps(input Observations) map[string]vlPlanLine {
	passes := make(map[string]OCRPass, len(input.Passes))
	for _, pass := range input.Passes {
		passes[pass.ID] = pass
	}
	lines := make(map[string]vlPlanLine, len(input.Lines))
	for _, line := range input.Lines {
		pass := passes[line.PassID]
		if pass.Status != "ok" || pass.RotationDegrees != 0 || pass.Kind == "mrz" ||
			(pass.Kind != "original" && pass.Kind != "document" && pass.Kind != "upper_page" && pass.Kind != "lower_page") ||
			isMRZLike(input, locatedLine{line: line, pass: pass}) {
			continue
		}
		box, ok := lineBounds(line)
		if !ok || box[0] < input.Document.Bounds[0] || box[1] < input.Document.Bounds[1] ||
			box[2] > input.Document.Bounds[2] || box[3] > input.Document.Bounds[3] {
			continue
		}
		x, y := center(line.Quad)
		lines[line.ID] = vlPlanLine{line: line, pass: pass, box: box, x: x, y: y, ids: []string{line.ID}}
	}
	return lines
}

func validBaselineTemplateRegions(input Observations, baseline Extraction) map[string]VLFieldRegion {
	regions := map[string]VLFieldRegion{}
	for _, field := range vlPlanFields {
		byGeometry := map[string]TemplateRegion{}
		var evidenceTemplateID string
		for _, candidate := range baseline.Fields[field].Candidates {
			for _, provenance := range candidate.TemplateEvidence {
				if provenance.Image != input.Image || provenance.TemplateID != templateID || provenance.Region.FieldID != field {
					continue
				}
				if evidenceTemplateID == "" {
					evidenceTemplateID = provenance.TemplateID
				}
				region := provenance.Region
				// Geometry validation must not depend on reader outputs. In
				// particular, row-crop alternatives may carry detector records
				// that do not match the primary mode. Validate a geometry-only
				// copy and leave baseline provenance untouched.
				region.Status = "empty"
				region.ElapsedSeconds = 0
				region.Records = nil
				region.Alternatives = nil
				geometryKey, err := json.Marshal(struct {
					TemplateID string            `json:"template_id"`
					Anchors    map[string]string `json:"anchors"`
					FieldID    string            `json:"field_id"`
					Crop       Rect              `json:"crop"`
					Mode       string            `json:"mode"`
					AnchorIDs  []string          `json:"anchor_ids"`
				}{provenance.TemplateID, provenance.Anchors, region.FieldID, region.Crop, region.Mode, region.AnchorIDs})
				if err != nil {
					continue
				}
				validation := &TemplateEvidence{SchemaVersion: TemplateEvidenceSchema, TemplateID: provenance.TemplateID, Image: input.Image, Anchors: provenance.Anchors, Regions: []TemplateRegion{region}}
				if err := validation.ValidateAgainst(input); err != nil {
					continue
				}
				byGeometry[string(geometryKey)] = region
			}
		}
		if len(byGeometry) != 1 || evidenceTemplateID != templateID {
			continue
		}
		var region TemplateRegion
		for _, geometry := range byGeometry {
			region = geometry
		}
		regions[field] = VLFieldRegion{ID: "vl-" + field, FieldID: field, Recipe: "registered_template_crop/v1", Crop: region.Crop, AnchorLineIDs: uniqueStrings(region.AnchorIDs)}
	}
	return regions
}

func vlPlanBaselineField(input Observations, baseline Extraction, field string, lines map[string]vlPlanLine, names *vlCoherentNameBlock) (VLFieldRegion, string) {
	if field == "issuer" {
		return vlAuthorityBlock(input, lines)
	}
	fieldResult, ok := baseline.Fields[field]
	if !ok {
		fieldResult = emptyField()
	}
	selected := vlFieldSeedLines(input, field, fieldResult, lines)
	if len(selected) > 0 {
		rows := vlGroupRows(selected)
		if len(rows) == 0 || !vlRowsAdjacent(rows) {
			return VLFieldRegion{}, "candidate_rows_ambiguous"
		}
		if field == "birth_place" {
			if names != nil && names.hasDOB {
				rows = vlRowsAfterAnchor(rows, names.dobBox)
				if len(rows) == 0 {
					return VLFieldRegion{}, "birth_place_seed_not_below_birth_date"
				}
			}
			rows = vlGrowTextBlock(rows, lines, input, field)
		}
		return vlCandidateRegion(input, field, flattenRows(rows), "baseline_candidate_rows/v1")
	}
	if field == "gender" || field == "birth_place" {
		return vlBirthDateNeighbor(input, baseline, field, lines, names)
	}
	return VLFieldRegion{}, "no_unambiguous_field_candidates"
}

func vlCandidateLineIDs(field FieldResult, input Observations) []string {
	ids := []string{}
	for _, candidate := range field.Candidates {
		ids = append(ids, candidate.LineIDs...)
		for _, provenance := range candidate.FieldEvidence {
			if provenance.ImageSHA256 == input.Image.SHA256 && provenance.ImageWidth == input.Image.Width && provenance.ImageHeight == input.Image.Height && provenance.GeometryScope != "" {
				ids = append(ids, provenance.SourceLineIDs...)
			}
		}
	}
	ids = append(ids, field.Evidence.LineIDs...)
	return uniqueStrings(ids)
}

func vlNameField(field string) bool {
	return field == "surname" || field == "given_name" || field == "patronymic"
}

func vlFieldSeedAllowed(input Observations, field string, line vlPlanLine) bool {
	text := strings.TrimSpace(line.line.TextRaw)
	if !hasLetters(text) || vlIsHeaderOrCaption(text) || vlDate(text) || vlDepartment(text) ||
		isMRZLike(input, locatedLine{line: line.line, pass: line.pass, x: line.x, y: line.y}) {
		return false
	}
	if field != "gender" && vlIsGenderText(text) {
		return false
	}
	if digits, numeric := numericToken(text); numeric && digits != "" {
		return false
	}
	if vlHasSerialLikeDigits(text) {
		return false
	}
	if vlNameField(field) {
		return vlNameLineAllowed(line)
	}
	if hasVLFieldLabel(text) {
		return vlHasOwnCaptionValue(field, text)
	}
	return true
}

func vlNameLineAllowed(line vlPlanLine) bool {
	text := strings.TrimSpace(line.line.TextRaw)
	nameText := cleanValue(text)
	if hasVLFieldLabel(text) {
		nameText = cleanValue(stripLabel(text, allLabelTerms()))
	}
	return nameLike(nameText) &&
		!vlIsHeaderOrCaption(text) && !vlIsGenderText(text) && !vlDate(text) && !vlDepartment(text)
}

func vlIsHeaderOrCaption(raw string) bool {
	if hasVLCaption(raw) {
		// A value line can contain the phrase "паспорт выдан". Keep it when
		// it also carries an authority value; a standalone passport caption is
		// still excluded. Exact-token matching avoids rejecting
		// "паспортно-визовым отделом" as a header.
		if hasVLIssuerLabel(raw) && hasAnyLabel(raw, authorityKeywords) {
			return false
		}
		return true
	}
	words := strings.FieldsFunc(raw, func(r rune) bool {
		return !((r >= 'А' && r <= 'Я') || (r >= 'а' && r <= 'я') || r == 'Ё' || r == 'ё' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	for i, word := range words {
		words[i] = canonicalLabel(word)
	}
	for i, word := range words {
		if word == "ПОДПИСЬ" || word == "ЛИЧНЫЙКОД" {
			return true
		}
		if i+1 < len(words) && ((word == "ЛИЧНАЯ" && words[i+1] == "ПОДПИСЬ") || (word == "ДЛЯ" && words[i+1] == "ОТМЕТОК")) {
			return true
		}
	}
	return false
}

func vlIsGenderText(raw string) bool {
	if _, ok := canonicalGender(cleanValue(raw)); ok {
		return true
	}
	_, ok := canonicalLayoutGender(cleanValue(raw))
	return ok
}

func vlHasSerialLikeDigits(raw string) bool {
	digitCount := 0
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digitCount++
		}
	}
	return digitCount >= 4
}

func vlHasOwnCaptionValue(field, raw string) bool {
	terms := labelTerms[field]
	return len(terms) > 0 && hasAnyLabel(raw, terms) && strings.TrimSpace(stripLabel(raw, terms)) != ""
}

func vlMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func vlFieldSeedLines(input Observations, field string, result FieldResult, lines map[string]vlPlanLine) []vlPlanLine {
	selected := []vlPlanLine{}
	for _, id := range vlCandidateLineIDs(result, input) {
		if line, exists := lines[id]; exists && vlFieldSeedAllowed(input, field, line) {
			selected = append(selected, line)
		}
	}
	selected = vlDeduplicateGeometry(selected)
	if len(selected) < 2 {
		return selected
	}
	heights := make([]float64, 0, len(selected))
	for _, line := range selected {
		heights = append(heights, line.box[3]-line.box[1])
	}
	median := vlMedian(heights)
	filtered := selected[:0]
	for _, line := range selected {
		h, w := line.box[3]-line.box[1], line.box[2]-line.box[0]
		if h <= 2.5*median && w >= .35*h {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func vlRowsAfterAnchor(rows [][]vlPlanLine, anchor Rect) [][]vlPlanLine {
	var after [][]vlPlanLine
	for _, row := range rows {
		if vlRowBounds(row)[1] >= anchor[3] {
			after = append(after, row)
		}
	}
	return after
}

func vlFindCoherentNameBlock(input Observations, baseline Extraction, lines map[string]vlPlanLine) (*vlCoherentNameBlock, string) {
	doc := input.Document.Bounds
	docWidth := doc[2] - doc[0]
	seam := pageSeam(input)
	seedIDs := map[string]bool{}
	placeIDs := map[string]bool{}
	for _, field := range []string{"surname", "given_name", "patronymic"} {
		for _, id := range vlCandidateLineIDs(baseline.Fields[field], input) {
			seedIDs[id] = true
		}
	}
	for _, id := range vlCandidateLineIDs(baseline.Fields["birth_place"], input) {
		placeIDs[id] = true
	}
	nameLines := []vlPlanLine{}
	dateLines := []vlPlanLine{}
	boundaryLines := []vlPlanLine{}
	for _, line := range lines {
		if pageOf(input, line.y) != "lower" || line.box[1] < seam || line.x < doc[0]+.42*docWidth {
			continue
		}
		if vlDate(line.line.TextRaw) {
			dateLines = append(dateLines, line)
		}
		if vlIsGenderText(line.line.TextRaw) || vlNameBoundaryLabel(line.line.TextRaw) || lineHasAnySeed(line, placeIDs) {
			boundaryLines = append(boundaryLines, line)
			continue
		}
		if vlNameLineAllowed(line) {
			nameLines = append(nameLines, line)
		}
	}
	nameLines = vlDeduplicateGeometry(nameLines)
	dateLines = vlDeduplicateDateLines(dateLines)
	boundaryLines = vlDeduplicateGeometry(boundaryLines)
	rows := vlGroupRows(nameLines)
	var blocks []*vlCoherentNameBlock
	for start := 0; start+3 <= len(rows); start++ {
		triplet := rows[start : start+3]
		if !vlCoherentNameRows(triplet, docWidth, seedIDs) || vlNameTripletCrossesBoundary(triplet, boundaryLines) {
			continue
		}
		// A DOB must be below the complete triplet. Overlapping duplicate DOB
		// boxes are collapsed to their compact representative before this check.
		if vlTripletHasDateAboveOrAcross(triplet, dateLines) {
			continue
		}
		block := &vlCoherentNameBlock{rows: triplet}
		if dob, ok := vlUniqueDOBBelowRows(triplet, dateLines); ok {
			block.dobIDs = append([]string(nil), dob.ids...)
			block.dobBox = dob.box
			block.hasDOB = true
		}
		blocks = append(blocks, block)
	}
	blocks = vlUniqueNameBlocks(blocks)
	blocks = vlBestSupportedNameBlocks(blocks, seedIDs)
	if len(blocks) != 1 {
		return nil, "coherent_name_rows_missing_or_ambiguous"
	}
	return blocks[0], ""
}

func vlBestSupportedNameBlocks(blocks []*vlCoherentNameBlock, seeds map[string]bool) []*vlCoherentNameBlock {
	if len(blocks) < 2 {
		return blocks
	}
	best := -1
	for _, block := range blocks {
		best = max(best, vlSeededNameRows(block.rows, seeds))
	}
	var selected []*vlCoherentNameBlock
	for _, block := range blocks {
		if vlSeededNameRows(block.rows, seeds) == best {
			selected = append(selected, block)
		}
	}
	return selected
}

func vlNameBoundaryLabel(raw string) bool {
	for _, field := range []string{"gender", "birth_date", "birth_place", "issuer", "issue_date", "department_code", "series", "number"} {
		if vlHasWholeLabel(raw, labelTerms[field]) {
			return true
		}
	}
	return vlHasAuthorityToken(raw)
}

func vlHasWholeLabel(raw string, terms []string) bool {
	words := strings.FieldsFunc(raw, func(r rune) bool {
		return !((r >= 'А' && r <= 'Я') || (r >= 'а' && r <= 'я') || r == 'Ё' || r == 'ё' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	for i := range words {
		joined := ""
		for j := i; j < len(words); j++ {
			joined += canonicalLabel(words[j])
			for _, term := range terms {
				if joined == term {
					return true
				}
			}
			if len(joined) > 32 {
				break
			}
		}
	}
	return false
}

func lineHasAnySeed(line vlPlanLine, seeds map[string]bool) bool {
	for _, id := range line.ids {
		if seeds[id] {
			return true
		}
	}
	return false
}

func vlSeededNameRows(rows [][]vlPlanLine, seeds map[string]bool) int {
	count := 0
	for _, row := range rows {
		for _, line := range row {
			if lineHasAnySeed(line, seeds) {
				count++
				break
			}
		}
	}
	return count
}

func vlTripletHasDateAboveOrAcross(rows [][]vlPlanLine, dates []vlPlanLine) bool {
	tripletBox := vlRowBounds(flattenRows(rows))
	for _, date := range dates {
		if date.box[1] <= tripletBox[3] {
			return true
		}
	}
	return false
}

func vlUniqueDOBBelowRows(rows [][]vlPlanLine, dates []vlPlanLine) (vlPlanLine, bool) {
	tripletBox := vlRowBounds(flattenRows(rows))
	var matches []vlPlanLine
	for _, date := range dates {
		if date.box[1] > tripletBox[3] {
			matches = append(matches, date)
		}
	}
	if len(matches) != 1 {
		return vlPlanLine{}, false
	}
	return matches[0], true
}

func vlNameTripletCrossesBoundary(rows [][]vlPlanLine, boundaries []vlPlanLine) bool {
	first, last := vlRowBounds(rows[0]), vlRowBounds(rows[len(rows)-1])
	for _, line := range boundaries {
		if line.y > first[3] && line.y < last[1] && vlHorizontalOverlap(first, line.box) {
			return true
		}
	}
	return false
}

func vlDeduplicateDateLines(lines []vlPlanLine) []vlPlanLine {
	ordered := append([]vlPlanLine(nil), lines...)
	sort.Slice(ordered, func(i, j int) bool {
		area := func(line vlPlanLine) float64 { return (line.box[2] - line.box[0]) * (line.box[3] - line.box[1]) }
		if area(ordered[i]) != area(ordered[j]) {
			return area(ordered[i]) < area(ordered[j])
		}
		return vlLineBefore(ordered[i], ordered[j])
	})
	var unique []vlPlanLine
	for _, line := range ordered {
		duplicate := false
		for i, previous := range unique {
			if vlDateBoxesOverlap(line.box, previous.box) {
				unique[i].ids = uniqueStrings(append(unique[i].ids, line.ids...))
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, line)
		}
	}
	return unique
}

func vlDateBoxesOverlap(a, b Rect) bool {
	intersectionWidth := math.Max(0, math.Min(a[2], b[2])-math.Max(a[0], b[0]))
	intersectionHeight := math.Max(0, math.Min(a[3], b[3])-math.Max(a[1], b[1]))
	intersection := intersectionWidth * intersectionHeight
	areaA := (a[2] - a[0]) * (a[3] - a[1])
	areaB := (b[2] - b[0]) * (b[3] - b[1])
	minimumArea := math.Min(areaA, areaB)
	if minimumArea <= 0 || intersection/minimumArea < .85 {
		return false
	}
	centerY := func(box Rect) float64 { return (box[1] + box[3]) / 2 }
	maxHeight := math.Max(a[3]-a[1], b[3]-b[1])
	return math.Abs(centerY(a)-centerY(b)) <= .5*maxHeight
}

func vlNameClusters(lines []vlPlanLine, docWidth float64) [][][]vlPlanLine {
	if len(lines) < 3 {
		return nil
	}
	heights := make([]float64, 0, len(lines))
	for _, line := range lines {
		heights = append(heights, line.box[3]-line.box[1])
	}
	median := vlMedian(heights)
	filtered := make([]vlPlanLine, 0, len(lines))
	for _, line := range lines {
		h, w := line.box[3]-line.box[1], line.box[2]-line.box[0]
		if h >= .55*median && h <= 1.8*median && w >= .35*h {
			filtered = append(filtered, line)
		}
	}
	rows := vlGroupRows(filtered)
	var clusters [][][]vlPlanLine
	var cluster [][]vlPlanLine
	for _, row := range rows {
		if len(cluster) == 0 {
			cluster = append(cluster, row)
			continue
		}
		previous, current := vlRowBounds(cluster[len(cluster)-1]), vlRowBounds(row)
		medianRowHeight := math.Max(vlRowHeight(cluster[len(cluster)-1]), vlRowHeight(row))
		gap := current[1] - previous[3]
		leftShift := math.Abs(current[0] - previous[0])
		if gap > 2.8*medianRowHeight || leftShift > .12*docWidth {
			clusters = append(clusters, cluster)
			cluster = [][]vlPlanLine{row}
			continue
		}
		cluster = append(cluster, row)
	}
	if len(cluster) > 0 {
		clusters = append(clusters, cluster)
	}
	return clusters
}

func vlCoherentNameRows(rows [][]vlPlanLine, docWidth float64, seedIDs map[string]bool) bool {
	if len(rows) != 3 {
		return false
	}
	lefts := []float64{}
	heights := []float64{}
	pitches := []float64{}
	seededRows := 0
	for i, row := range rows {
		box := vlRowBounds(row)
		lefts = append(lefts, box[0])
		heights = append(heights, box[3]-box[1])
		seeded := false
		for _, line := range row {
			for _, id := range line.ids {
				if seedIDs[id] {
					seeded = true
				}
			}
		}
		if seeded {
			seededRows++
		}
		if i > 0 {
			previous := vlRowBounds(rows[i-1])
			pitch := (box[1]+box[3])/2 - (previous[1]+previous[3])/2
			if pitch <= 0 {
				return false
			}
			pitches = append(pitches, pitch)
		}
	}
	minHeight, maxHeight := heights[0], heights[0]
	for _, height := range heights[1:] {
		minHeight, maxHeight = math.Min(minHeight, height), math.Max(maxHeight, height)
	}
	if seededRows < 2 || maxHeight/minHeight > 1.8 || maxSliceFloat(lefts)-minSliceFloat(lefts) > .12*docWidth {
		return false
	}
	if len(pitches) != 2 || pitches[0] < .75*vlMedian(heights) || pitches[1] < .75*vlMedian(heights) ||
		math.Max(pitches[0], pitches[1]) > 2.5*math.Min(pitches[0], pitches[1]) {
		return false
	}
	return true
}

func vlUniqueNameBlocks(blocks []*vlCoherentNameBlock) []*vlCoherentNameBlock {
	sort.Slice(blocks, func(i, j int) bool {
		return vlLineBefore(blocks[i].rows[0][0], blocks[j].rows[0][0])
	})
	var unique []*vlCoherentNameBlock
	for _, block := range blocks {
		duplicate := false
		for _, previous := range unique {
			if vlRowBounds(block.rows[0]) == vlRowBounds(previous.rows[0]) &&
				vlRowBounds(block.rows[1]) == vlRowBounds(previous.rows[1]) &&
				vlRowBounds(block.rows[2]) == vlRowBounds(previous.rows[2]) &&
				block.hasDOB == previous.hasDOB && (!block.hasDOB || vlSameGlyph(block.dobBox, previous.dobBox)) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, block)
		}
	}
	return unique
}

func vlNameFieldRegion(input Observations, field string, block *vlCoherentNameBlock) (VLFieldRegion, string) {
	if block == nil || len(block.rows) != 3 {
		return VLFieldRegion{}, "coherent_name_rows_missing_or_ambiguous"
	}
	index := map[string]int{"surname": 0, "given_name": 1, "patronymic": 2}[field]
	row := block.rows[index]
	box := vlRowBounds(row)
	heights := make([]float64, 0, len(row))
	anchors := []string{}
	for _, candidateRow := range block.rows {
		for _, line := range candidateRow {
			anchors = append(anchors, line.ids...)
		}
	}
	anchors = append(anchors, block.dobIDs...)
	for _, line := range row {
		heights = append(heights, line.box[3]-line.box[1])
	}
	h := vlMedian(heights)
	raw := Rect{box[0] - 4*h, box[1] - .2*h, box[2] + 4*h, box[3] + .2*h}
	if block.hasDOB {
		raw[3] = math.Min(raw[3], block.dobBox[1]-1)
	}
	crop, ok := vlIntegerCrop(raw, input)
	if !ok || !vlRectContains(crop, box) {
		return VLFieldRegion{}, "name_row_crop_unavailable"
	}
	return VLFieldRegion{ID: "vl-" + field, FieldID: field, Recipe: "coherent_name_rows/v1", Crop: crop, AnchorLineIDs: uniqueStrings(anchors)}, ""
}

func maxSliceFloat(values []float64) float64 {
	maximum := math.Inf(-1)
	for _, value := range values {
		maximum = math.Max(maximum, value)
	}
	return maximum
}

func minSliceFloat(values []float64) float64 {
	minimum := math.Inf(1)
	for _, value := range values {
		minimum = math.Min(minimum, value)
	}
	return minimum
}

func vlPlanLabelOnly(field, raw string) bool {
	terms := labelTerms[field]
	if len(terms) == 0 || !hasAnyLabel(raw, terms) {
		return false
	}
	return stripLabel(raw, terms) == ""
}

func vlDeduplicateGeometry(lines []vlPlanLine) []vlPlanLine {
	sort.Slice(lines, func(i, j int) bool { return vlLineBefore(lines[i], lines[j]) })
	var out []vlPlanLine
	for _, line := range lines {
		match := -1
		for i := range out {
			if vlSameGlyph(line.box, out[i].box) {
				match = i
				break
			}
		}
		if match < 0 {
			out = append(out, line)
			continue
		}
		out[match].ids = append(out[match].ids, line.ids...)
		out[match].box = vlUnion(out[match].box, line.box)
	}
	return out
}

func vlSameGlyph(a, b Rect) bool {
	intersection := math.Max(0, math.Min(a[2], b[2])-math.Max(a[0], b[0])) * math.Max(0, math.Min(a[3], b[3])-math.Max(a[1], b[1]))
	areaA, areaB := (a[2]-a[0])*(a[3]-a[1]), (b[2]-b[0])*(b[3]-b[1])
	if intersection > 0 && intersection/math.Min(areaA, areaB) >= .55 {
		return true
	}
	ax, ay := (a[0]+a[2])/2, (a[1]+a[3])/2
	bx, by := (b[0]+b[2])/2, (b[1]+b[3])/2
	h := math.Max(a[3]-a[1], b[3]-b[1])
	return math.Hypot(ax-bx, ay-by) <= .35*h && math.Abs((a[3]-a[1])-(b[3]-b[1])) <= .5*h
}

func vlGroupRows(lines []vlPlanLine) [][]vlPlanLine {
	sort.Slice(lines, func(i, j int) bool { return vlLineBefore(lines[i], lines[j]) })
	var rows [][]vlPlanLine
	for _, line := range lines {
		if len(rows) == 0 || math.Abs(line.y-vlRowCenter(rows[len(rows)-1])) > .6*math.Max(line.box[3]-line.box[1], vlRowHeight(rows[len(rows)-1])) {
			rows = append(rows, []vlPlanLine{line})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], line)
		}
	}
	return rows
}

func vlLineBefore(a, b vlPlanLine) bool {
	if a.y != b.y {
		return a.y < b.y
	}
	if a.x != b.x {
		return a.x < b.x
	}
	if a.pass.ID != b.pass.ID {
		if vlPassPriority(a.pass) != vlPassPriority(b.pass) {
			return vlPassPriority(a.pass) < vlPassPriority(b.pass)
		}
		return a.pass.ID < b.pass.ID
	}
	return a.line.ID < b.line.ID
}

func vlPassPriority(pass OCRPass) int {
	switch pass.Kind {
	case "original":
		return 0
	case "upper_page", "lower_page":
		return 1
	case "document":
		return 2
	default:
		return 3
	}
}

func vlRowCenter(row []vlPlanLine) float64 {
	box := vlRowBounds(row)
	return (box[1] + box[3]) / 2
}

func vlRowHeight(row []vlPlanLine) float64 {
	box := vlRowBounds(row)
	return box[3] - box[1]
}

func vlRowBounds(row []vlPlanLine) Rect {
	box := row[0].box
	for _, line := range row[1:] {
		box = vlUnion(box, line.box)
	}
	return box
}

func vlRowsAdjacent(rows [][]vlPlanLine) bool {
	for i := 1; i < len(rows); i++ {
		a, b := vlRowBounds(rows[i-1]), vlRowBounds(rows[i])
		gap := b[1] - a[3]
		if gap > 1.6*math.Max(a[3]-a[1], b[3]-b[1]) || !vlHorizontalOverlap(a, b) {
			return false
		}
	}
	return true
}

func vlHorizontalOverlap(a, b Rect) bool {
	return math.Min(a[2], b[2])-math.Max(a[0], b[0]) > -.5*math.Max(a[3]-a[1], b[3]-b[1])
}

func flattenRows(rows [][]vlPlanLine) []vlPlanLine {
	var out []vlPlanLine
	for _, row := range rows {
		out = append(out, row...)
	}
	return out
}

func vlGrowTextBlock(rows [][]vlPlanLine, all map[string]vlPlanLine, input Observations, field string) [][]vlPlanLine {
	seedLines := flattenRows(rows)
	if len(seedLines) == 0 {
		return rows
	}
	seedBox := vlRowBounds(seedLines)
	lineHeights := make([]float64, 0, len(seedLines))
	for _, line := range seedLines {
		lineHeights = append(lineHeights, line.box[3]-line.box[1])
	}
	sort.Float64s(lineHeights)
	seedHeight := lineHeights[len(lineHeights)/2]
	if len(lineHeights)%2 == 0 {
		seedHeight = (lineHeights[len(lineHeights)/2-1] + seedHeight) / 2
	}
	passID := seedLines[0].pass.ID
	seedPage := pageOf(input, (seedBox[1]+seedBox[3])/2)
	selected := map[string]bool{}
	for _, line := range seedLines {
		selected[line.line.ID] = true
	}
	candidates := make([]vlPlanLine, 0, len(all))
	for id, line := range all {
		if selected[id] || line.pass.ID != passID || pageOf(input, line.y) != seedPage || !vlExpandableLine(input, field, line, seedHeight) {
			continue
		}
		candidates = append(candidates, line)
	}
	sort.Slice(candidates, func(i, j int) bool { return vlLineBefore(candidates[i], candidates[j]) })
	var above, below []vlPlanLine
	for _, line := range candidates {
		if !vlHorizontalOverlap(seedBox, line.box) || math.Abs(line.box[3]-line.box[1]) > 2.5*seedHeight {
			continue
		}
		gap := math.Max(seedBox[1]-line.box[3], line.box[1]-seedBox[3])
		if gap > 1.5*seedHeight || vlHasDateOrCodeBarrier(line, seedBox, all) || vlHasMRZBarrier(input, line, seedBox) {
			continue
		}
		if line.y < seedBox[1] && field != "birth_place" {
			above = append(above, line)
		} else if line.y > seedBox[3] {
			below = append(below, line)
		}
	}
	// Only the nearest aligned adjacent rows may extend the region, and the
	// total remains bounded to four rows around the original seed.
	sort.Slice(above, func(i, j int) bool { return vlLineBefore(above[j], above[i]) })
	selectedRows := append([][]vlPlanLine(nil), rows...)
	for _, line := range above {
		if len(selectedRows) >= 4 || !vlRowGrowthAdjacent(selectedRows[0], []vlPlanLine{line}, seedHeight) {
			break
		}
		selectedRows = append([][]vlPlanLine{{line}}, selectedRows...)
	}
	sort.Slice(below, func(i, j int) bool { return vlLineBefore(below[i], below[j]) })
	for _, line := range below {
		if len(selectedRows) >= 4 || !vlRowGrowthAdjacent(selectedRows[len(selectedRows)-1], []vlPlanLine{line}, seedHeight) {
			break
		}
		selectedRows = append(selectedRows, []vlPlanLine{line})
	}
	return selectedRows
}

func vlExpandableLine(input Observations, field string, line vlPlanLine, seedHeight float64) bool {
	height, width := line.box[3]-line.box[1], line.box[2]-line.box[0]
	return vlFieldSeedAllowed(input, field, line) && height <= 2.5*seedHeight && width >= .35*height
}

func vlRowGrowthAdjacent(a, b []vlPlanLine, lineHeight float64) bool {
	boxA, boxB := vlRowBounds(a), vlRowBounds(b)
	gap := math.Max(boxA[1]-boxB[3], boxB[1]-boxA[3])
	return gap <= 1.5*lineHeight && vlHorizontalOverlap(boxA, boxB)
}

func vlHasDateOrCodeBarrier(candidate vlPlanLine, current Rect, all map[string]vlPlanLine) bool {
	low, high := math.Min(candidate.y, (current[1]+current[3])/2), math.Max(candidate.y, (current[1]+current[3])/2)
	for _, line := range all {
		if line.pass.ID != candidate.pass.ID || (!vlDate(line.line.TextRaw) && !vlDepartment(line.line.TextRaw)) || !vlHorizontalOverlap(current, line.box) {
			continue
		}
		if line.y > low && line.y < high {
			return true
		}
	}
	return false
}

func vlHasMRZBarrier(input Observations, candidate vlPlanLine, current Rect) bool {
	low, high := math.Min(candidate.y, (current[1]+current[3])/2), math.Max(candidate.y, (current[1]+current[3])/2)
	for _, line := range input.Lines {
		pass := mapPass(input.Passes, line.PassID)
		if pass.Kind != "mrz" && !looksLikeMRZLine(line, input.Document.Bounds) {
			continue
		}
		box, ok := lineBounds(line)
		if ok && (box[1]+box[3])/2 > low && (box[1]+box[3])/2 < high && vlHorizontalOverlap(current, box) {
			return true
		}
	}
	return false
}

func vlCandidateRegion(input Observations, field string, lines []vlPlanLine, recipe string) (VLFieldRegion, string) {
	if len(lines) == 0 {
		return VLFieldRegion{}, "no_unambiguous_field_candidates"
	}
	box := vlRowBounds(lines)
	lineHeights := make([]float64, 0, len(lines))
	for _, line := range lines {
		lineHeights = append(lineHeights, line.box[3]-line.box[1])
	}
	sort.Float64s(lineHeights)
	h := lineHeights[len(lineHeights)/2]
	if len(lineHeights)%2 == 0 {
		h = (lineHeights[len(lineHeights)/2-1] + h) / 2
	}
	padY, padX := math.Max(2, .35*h), math.Max(3, .5*h)
	if field == "birth_place" {
		padX = math.Max(padX, 3*h)
		padY = math.Max(padY, .5*h)
	}
	crop, ok := vlIntegerCrop(Rect{box[0] - padX, box[1] - padY, box[2] + padX, box[3] + padY}, input)
	if !ok || !vlRectContains(crop, box) {
		return VLFieldRegion{}, "integer_crop_unavailable"
	}
	anchors := []string{}
	for _, line := range lines {
		anchors = append(anchors, line.ids...)
	}
	return VLFieldRegion{ID: "vl-" + field, FieldID: field, Recipe: recipe, Crop: crop, AnchorLineIDs: uniqueStrings(anchors)}, ""
}

func vlBirthDateNeighbor(input Observations, baseline Extraction, field string, lines map[string]vlPlanLine, names *vlCoherentNameBlock) (VLFieldRegion, string) {
	dob, support, ok := vlBirthDateAnchor(input, baseline, lines, names)
	if !ok {
		return VLFieldRegion{}, "birth_date_anchor_missing_or_ambiguous"
	}
	doc := input.Document.Bounds
	dobHeight := dob.box[3] - dob.box[1]
	var raw Rect
	if field == "gender" {
		raw = Rect{dob.box[0] - 8*dobHeight, dob.box[1] - .25*dobHeight, dob.box[0] - dobHeight, dob.box[3] + .25*dobHeight}
	} else {
		left := dob.box[0] - 8*dobHeight
		right := math.Min(doc[2]-dobHeight, dob.box[2]+dobHeight)
		bottom := math.Min(doc[3], dob.box[3]+5*dobHeight)
		for _, line := range input.Lines {
			pass := mapPass(input.Passes, line.PassID)
			if pass.Kind == "mrz" {
				b, ok := lineBounds(line)
				if ok && b[1] > dob.box[3] {
					bottom = math.Min(bottom, b[1]-.25*dobHeight)
				}
			}
		}
		raw = Rect{left, dob.box[3] + .2*dobHeight, right, bottom}
	}
	raw[0] = math.Max(raw[0], doc[0])
	raw[1] = math.Max(raw[1], doc[1])
	raw[2] = math.Min(raw[2], doc[2])
	raw[3] = math.Min(raw[3], doc[3])
	crop, ok := vlIntegerCrop(raw, input)
	if !ok {
		return VLFieldRegion{}, "neighbor_crop_unavailable"
	}
	anchors := append(append([]string(nil), dob.ids...), support...)
	return VLFieldRegion{ID: "vl-" + field, FieldID: field, Recipe: "birth_date_neighbor/v1", Crop: crop, AnchorLineIDs: uniqueStrings(anchors)}, ""
}

func vlBirthDateAnchor(input Observations, baseline Extraction, lines map[string]vlPlanLine, names *vlCoherentNameBlock) (vlPlanLine, []string, bool) {
	if names != nil && names.hasDOB {
		support := []string{}
		for _, row := range names.rows {
			for _, line := range row {
				support = append(support, line.ids...)
			}
		}
		return vlPlanLine{box: names.dobBox, ids: names.dobIDs}, uniqueStrings(support), true
	}
	nameSupportAbove := func(dob vlPlanLine, minimum int) []string {
		candidates := []vlPlanLine{}
		for _, field := range []string{"surname", "given_name", "patronymic"} {
			for _, id := range vlCandidateLineIDs(baseline.Fields[field], input) {
				if line, exists := lines[id]; exists && pageOf(input, line.y) == "lower" && line.box[3] < dob.box[1] && vlNameLineAllowed(line) {
					candidates = append(candidates, line)
				}
			}
		}
		candidates = vlDeduplicateGeometry(candidates)
		if len(candidates) < minimum {
			return nil
		}
		ids := []string{}
		for _, line := range candidates {
			ids = append(ids, line.ids...)
		}
		return uniqueStrings(ids)
	}
	if result, exists := baseline.Fields["birth_date"]; exists {
		candidates := []vlPlanLine{}
		for _, id := range vlCandidateLineIDs(result, input) {
			if line, found := lines[id]; found && pageOf(input, line.y) == "lower" && vlDate(line.line.TextRaw) {
				candidates = append(candidates, line)
			}
		}
		candidates = vlDeduplicateGeometry(candidates)
		if len(candidates) == 1 {
			if support := nameSupportAbove(candidates[0], 1); len(support) > 0 {
				return candidates[0], support, true
			}
		}
	}
	// If baseline DOB assignment is absent, permit a lower-page date only
	// when a multi-line FIO candidate context independently supports it.
	allDates := []vlPlanLine{}
	for _, line := range lines {
		if pageOf(input, line.y) == "lower" && vlDate(line.line.TextRaw) {
			allDates = append(allDates, line)
		}
	}
	allDates = vlDeduplicateGeometry(allDates)
	if len(allDates) == 1 {
		if support := nameSupportAbove(allDates[0], 2); len(support) > 0 {
			return allDates[0], support, true
		}
	}
	return vlPlanLine{}, nil, false
}

func vlAuthorityBlock(input Observations, lines map[string]vlPlanLine) (VLFieldRegion, string) {
	byPass := map[string][]vlPlanLine{}
	for _, line := range lines {
		if pageOf(input, line.y) == "upper" && vlHasAuthorityToken(line.line.TextRaw) && vlFieldSeedAllowed(input, "issuer", line) {
			byPass[line.pass.ID] = append(byPass[line.pass.ID], line)
		} else if pageOf(input, line.y) == "upper" && vlHasOwnCaptionValue("issuer", line.line.TextRaw) && vlFieldSeedAllowed(input, "issuer", line) {
			byPass[line.pass.ID] = append(byPass[line.pass.ID], line)
		}
	}
	var clusters [][]vlPlanLine
	passIDs := make([]string, 0, len(byPass))
	for passID := range byPass {
		passIDs = append(passIDs, passID)
	}
	sort.Strings(passIDs)
	for _, passID := range passIDs {
		anchors := byPass[passID]
		rows := vlGroupRows(anchors)
		var cluster [][]vlPlanLine
		for _, row := range rows {
			if len(cluster) == 0 || vlRowsAdjacent([][]vlPlanLine{cluster[len(cluster)-1], row}) {
				cluster = append(cluster, row)
				continue
			}
			clusters = append(clusters, flattenRows(cluster))
			cluster = [][]vlPlanLine{row}
		}
		if len(cluster) > 0 {
			clusters = append(clusters, flattenRows(cluster))
		}
	}
	clusters = vlDeduplicateLineClusters(clusters)
	if len(clusters) != 1 || len(vlGroupRows(clusters[0])) > 4 {
		return VLFieldRegion{}, "authority_block_missing_or_ambiguous"
	}
	rows := vlGrowIssuerBlock(vlGroupRows(clusters[0]), lines, input)
	if len(rows) > 4 {
		return VLFieldRegion{}, "authority_block_too_large"
	}
	return vlCandidateRegion(input, "issuer", flattenRows(rows), "authority_text_block/v1")
}

func vlHasAuthorityToken(raw string) bool {
	words := strings.FieldsFunc(raw, func(r rune) bool {
		return !((r >= 'А' && r <= 'Я') || (r >= 'а' && r <= 'я') || r == 'Ё' || r == 'ё' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	for _, word := range words {
		canonical := canonicalLabel(word)
		for _, term := range authorityKeywords {
			if canonical == term || ((term == "ОТДЕЛ" || term == "УПРАВЛЕНИ") && strings.HasPrefix(canonical, term)) {
				return true
			}
		}
	}
	return false
}

func vlGrowIssuerBlock(rows [][]vlPlanLine, all map[string]vlPlanLine, input Observations) [][]vlPlanLine {
	seedLines := flattenRows(rows)
	if len(seedLines) == 0 {
		return rows
	}
	seedBox := vlRowBounds(seedLines)
	heights := make([]float64, 0, len(seedLines))
	for _, line := range seedLines {
		heights = append(heights, line.box[3]-line.box[1])
	}
	seedHeight := vlMedian(heights)
	passID := seedLines[0].pass.ID
	page := pageOf(input, (seedBox[1]+seedBox[3])/2)
	selected := map[string]bool{}
	for _, line := range seedLines {
		selected[line.line.ID] = true
	}
	var below []vlPlanLine
	for id, line := range all {
		if selected[id] || line.pass.ID != passID || pageOf(input, line.y) != page || line.line.TextRaw == "" ||
			!vlFieldSeedAllowed(input, "issuer", line) || !vlHorizontalOverlap(seedBox, line.box) ||
			line.box[3]-line.box[1] > 2.5*seedHeight || line.y <= seedBox[3] {
			continue
		}
		if vlHasDateOrCodeBarrier(line, seedBox, all) || vlHasMRZBarrier(input, line, seedBox) {
			continue
		}
		below = append(below, line)
	}
	sort.Slice(below, func(i, j int) bool { return vlLineBefore(below[i], below[j]) })
	selectedRows := append([][]vlPlanLine(nil), rows...)
	for _, line := range below {
		if len(selectedRows) >= 4 {
			break
		}
		last := selectedRows[len(selectedRows)-1]
		lastBox := vlRowBounds(last)
		lastCenter := (lastBox[1] + lastBox[3]) / 2
		if line.y <= lastCenter || !vlRowGrowthAdjacent(last, []vlPlanLine{line}, seedHeight) {
			break
		}
		selectedRows = append(selectedRows, []vlPlanLine{line})
	}
	return selectedRows
}

func vlDeduplicateLineClusters(clusters [][]vlPlanLine) [][]vlPlanLine {
	sort.Slice(clusters, func(i, j int) bool {
		return vlLineBefore(clusters[i][0], clusters[j][0])
	})
	var unique [][]vlPlanLine
	for _, cluster := range clusters {
		box := vlRowBounds(cluster)
		duplicate := false
		for _, previous := range unique {
			other := vlRowBounds(previous)
			if vlSameGlyph(box, other) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, cluster)
		}
	}
	return unique
}

func vlDate(raw string) bool {
	indices := vlDateToken.FindAllStringIndex(raw, -1)
	valid := 0
	for _, index := range indices {
		if (index[0] > 0 && raw[index[0]-1] >= '0' && raw[index[0]-1] <= '9') ||
			(index[1] < len(raw) && raw[index[1]] >= '0' && raw[index[1]] <= '9') {
			continue
		}
		match := raw[index[0]:index[1]]
		fields := strings.Fields(match)
		normalized := strings.ReplaceAll(strings.Join(fields, ""), "/", ".")
		if len(fields) == 3 && !strings.ContainsAny(match, "./-") {
			normalized = fields[0] + "." + fields[1] + "." + fields[2]
		}
		if strings.Contains(normalized, "-") && len(normalized) == 10 && normalized[4] == '-' {
			if _, err := time.Parse("2006-01-02", normalized); err == nil {
				valid++
			}
		} else if _, ok := parseDate(normalized); ok {
			valid++
		}
	}
	return valid == 1
}

func vlDepartment(raw string) bool {
	canonical := canonicalDepartment(strings.TrimSpace(raw))
	return deptDash.MatchString(canonical)
}

func vlIntegerCrop(raw Rect, input Observations) (Rect, bool) {
	doc := input.Document.Bounds
	x0, y0 := math.Ceil(doc[0]), math.Ceil(doc[1])
	x1, y1 := math.Floor(doc[2]), math.Floor(doc[3])
	if x0 >= x1 || y0 >= y1 {
		return Rect{}, false
	}
	crop := Rect{
		math.Max(x0, math.Floor(raw[0])), math.Max(y0, math.Floor(raw[1])),
		math.Min(x1, math.Ceil(raw[2])), math.Min(y1, math.Ceil(raw[3])),
	}
	return crop, validRect(crop, float64(input.Image.Width), float64(input.Image.Height))
}

func vlRectContains(outer, inner Rect) bool {
	return outer[0] <= inner[0] && outer[1] <= inner[1] && outer[2] >= inner[2] && outer[3] >= inner[3]
}

func vlUnion(a, b Rect) Rect {
	return Rect{math.Min(a[0], b[0]), math.Min(a[1], b[1]), math.Max(a[2], b[2]), math.Max(a[3], b[3])}
}

func mapPass(passes []OCRPass, id string) OCRPass {
	for _, pass := range passes {
		if pass.ID == id {
			return pass
		}
	}
	return OCRPass{}
}
