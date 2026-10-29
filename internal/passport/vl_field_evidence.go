package passport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"
	"unicode"
)

const VLFieldEvidenceSchema = "passport-vl-field-evidence/v1"

// VLFieldEvidence contains private OCR results for crops chosen by the Go
// planner. It never manufactures observations lines or word-level geometry.
type VLFieldEvidence struct {
	SchemaVersion string             `json:"schema_version"`
	Image         ImageObservation   `json:"image"`
	PlanSHA256    string             `json:"plan_sha256"`
	Reader        VLReaderDescriptor `json:"reader"`
	Regions       []VLFieldRead      `json:"regions"`
}

type VLReaderDescriptor struct {
	ModelID            string `json:"model_id"`
	ModelTreeSHA256    string `json:"model_tree_sha256"`
	Backend            string `json:"backend"`
	Device             string `json:"device"`
	PipelineVersion    string `json:"pipeline_version"`
	UseLayoutDetection bool   `json:"use_layout_detection"`
	UseDocPreprocessor bool   `json:"use_doc_preprocessor"`
}

type VLFieldRead struct {
	RegionID       string         `json:"region_id"`
	Region         VLFieldRegion  `json:"region"`
	Status         string         `json:"status"`
	Blocks         []VLFieldBlock `json:"blocks"`
	GeometryScope  string         `json:"geometry_scope"`
	CapReached     *bool          `json:"cap_reached"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
}

type VLFieldBlock struct {
	TextRaw string `json:"text_raw"`
}

type VLFieldOptions struct {
	ModelTreeSHA256 string
	Policy          string // "union" (default) or experimental "primary".
}

type VLFieldProvenance struct {
	Image      ImageObservation   `json:"image"`
	PlanSHA256 string             `json:"plan_sha256"`
	Region     VLFieldRegion      `json:"region"`
	Reader     VLReaderDescriptor `json:"reader"`
	Read       VLFieldRead        `json:"read"`
}

func (e *VLFieldEvidence) UnmarshalJSON(data []byte) error {
	type wire VLFieldEvidence
	if err := requireJSONFields(data, "schema_version", "image", "plan_sha256", "reader", "regions"); err != nil {
		return err
	}
	var parsed wire
	if err := decodeStrictJSON(data, &parsed); err != nil {
		return err
	}
	*e = VLFieldEvidence(parsed)
	return nil
}

func (r *VLReaderDescriptor) UnmarshalJSON(data []byte) error {
	type wire VLReaderDescriptor
	if err := requireJSONFields(data, "model_id", "model_tree_sha256", "backend", "device", "pipeline_version", "use_layout_detection", "use_doc_preprocessor"); err != nil {
		return err
	}
	var parsed wire
	if err := decodeStrictJSON(data, &parsed); err != nil {
		return err
	}
	*r = VLReaderDescriptor(parsed)
	return nil
}

func (r *VLFieldRead) UnmarshalJSON(data []byte) error {
	type wire VLFieldRead
	if err := requireJSONFields(data, "region_id", "region", "status", "blocks", "geometry_scope", "cap_reached", "elapsed_seconds"); err != nil {
		return err
	}
	var parsed wire
	if err := decodeStrictJSON(data, &parsed); err != nil {
		return err
	}
	*r = VLFieldRead(parsed)
	return nil
}

func requireJSONFields(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range names {
		if _, exists := fields[name]; !exists {
			return errors.New("missing required VL evidence field")
		}
	}
	return nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple VL evidence JSON values")
	}
	return nil
}

const (
	vlModelID         = "PaddleOCR-VL-1.6-0.9B"
	vlPipelineVersion = "v1.6"
	vlGeometryScope   = "whole_crop_synthetic_prompt_box"
	vlMaxRegions      = 6
	vlMaxBlocks       = 8
	vlMaxTextBytes    = 4096
)

// ApplyVLFieldEvidence validates a reader handoff against a freshly computed
// Go plan, then merges eligible whole-field proposals. All outcomes require
// manual review; no VL score or token count is treated as confidence.
func ApplyVLFieldEvidence(input Observations, opts Options, baseline Extraction, evidence *VLFieldEvidence, vlOpts VLFieldOptions) (Extraction, error) {
	if evidence == nil {
		return baseline, nil
	}
	if err := input.Validate(); err != nil {
		return Extraction{}, err
	}
	if baseline.ImageID != input.Image.ID {
		return Extraction{}, errors.New("VL baseline image mismatch")
	}
	if vlOpts.Policy == "" {
		vlOpts.Policy = "union"
	}
	if vlOpts.Policy != "union" && vlOpts.Policy != "primary" {
		return Extraction{}, errors.New("unsupported VL field policy")
	}
	if !fieldEvidenceSHA.MatchString(vlOpts.ModelTreeSHA256) || vlOpts.ModelTreeSHA256 != evidence.Reader.ModelTreeSHA256 {
		return Extraction{}, errors.New("VL model tree identity mismatch")
	}
	if evidence.SchemaVersion != VLFieldEvidenceSchema || evidence.Image != input.Image || !fieldEvidenceSHA.MatchString(evidence.PlanSHA256) {
		return Extraction{}, errors.New("invalid VL evidence identity")
	}
	if err := validateVLReader(evidence.Reader); err != nil {
		return Extraction{}, err
	}
	plan, err := BuildVLFieldPlan(input, baseline)
	if err != nil {
		return Extraction{}, err
	}
	planSHA, err := VLFieldPlanSHA256(plan)
	if err != nil {
		return Extraction{}, err
	}
	if evidence.PlanSHA256 != planSHA || plan.PlanSHA256 != planSHA || plan.Image != input.Image || !fieldEvidenceSHA.MatchString(plan.BaselineSHA256) {
		return Extraction{}, errors.New("VL field plan mismatch")
	}
	if len(plan.Regions) > vlMaxRegions || len(evidence.Regions) != len(plan.Regions) {
		return Extraction{}, errors.New("VL evidence region set mismatch")
	}
	planned := make(map[string]VLFieldRegion, len(plan.Regions))
	for _, region := range plan.Regions {
		if region.ID == "" || !vlFieldAllowed(region.FieldID) {
			return Extraction{}, errors.New("invalid Go-planned VL region")
		}
		if _, exists := planned[region.ID]; exists {
			return Extraction{}, errors.New("duplicate Go-planned VL region")
		}
		planned[region.ID] = region
	}
	reads := make(map[string]VLFieldRead, len(evidence.Regions))
	for _, read := range evidence.Regions {
		region, exists := planned[read.RegionID]
		if !exists {
			return Extraction{}, errors.New("unknown VL evidence region")
		}
		if _, exists := reads[read.RegionID]; exists {
			return Extraction{}, errors.New("duplicate VL evidence region")
		}
		if !sameVLRegion(region, read.Region) {
			return Extraction{}, errors.New("VL evidence region differs from Go plan")
		}
		if err := validateVLRead(read); err != nil {
			return Extraction{}, err
		}
		reads[read.RegionID] = read
	}
	for id := range planned {
		if _, exists := reads[id]; !exists {
			return Extraction{}, errors.New("missing VL evidence region")
		}
	}

	result := cloneExtraction(baseline)
	result.ReviewRequired = true
	for _, region := range plan.Regions {
		read := reads[region.ID]
		field := result.Fields[region.FieldID]
		field.Issues = appendUnique(field.Issues, "vl_field_candidate_requires_manual_review")
		if read.Status == "error" {
			field.Issues = appendUnique(field.Issues, "vl_reader_error_no_proposal")
			result.Fields[region.FieldID] = field
			continue
		}
		if read.Status == "empty" {
			field.Issues = appendUnique(field.Issues, "vl_reader_empty_no_proposal")
			result.Fields[region.FieldID] = field
			continue
		}
		if read.CapReached != nil && *read.CapReached {
			field.Issues = appendUnique(field.Issues, "vl_generation_cap_reached_no_proposal")
			result.Fields[region.FieldID] = field
			continue
		}
		if read.CapReached == nil {
			field.Issues = appendUnique(field.Issues, "vl_generation_cap_unknown_requires_review")
		}
		raw := strings.Join(strings.Fields(joinVLBlocks(read.Blocks)), " ")
		value, rules, ok := vlFieldValue(region.FieldID, raw, opts.NormalizeNames)
		if !ok {
			field.Issues = appendUnique(field.Issues, "vl_reader_text_not_eligible_for_proposal")
			result.Fields[region.FieldID] = field
			continue
		}
		provenance := VLFieldProvenance{Image: evidence.Image, PlanSHA256: evidence.PlanSHA256, Region: region, Reader: evidence.Reader, Read: read}
		candidate := Candidate{
			Value: value, Raw: raw, RawVariants: []string{raw},
			Rules:   uniqueStrings(append(rules, "vl_whole_field_candidate")),
			Context: "vl_field_crop:review_only", VLEvidence: []VLFieldProvenance{provenance},
		}
		field.Candidates = append(field.Candidates, candidate)
		if vlOpts.Policy == "primary" {
			for _, existing := range field.Candidates {
				if existing.Value != candidate.Value {
					field.Issues = appendUnique(field.Issues, "vl_viz_conflict_requires_review")
					break
				}
			}
			field = resolveCandidateSet(input, region.FieldID, []Candidate{candidate}, field.Issues, true)
			// Keep every incumbent candidate visible alongside the experimental
			// primary suggestion, even when it differs.
			field.Candidates = mergeCandidateSets(field.Candidates, baseline.Fields[region.FieldID].Candidates)
			field.Evidence = Evidence{LineIDs: []string{}, PassIDs: []string{}}
			for _, retained := range field.Candidates {
				field.Evidence.LineIDs = uniqueStrings(append(field.Evidence.LineIDs, retained.LineIDs...))
				field.Evidence.PassIDs = uniqueStrings(append(field.Evidence.PassIDs, retained.PassIDs...))
			}
			if len(field.Candidates) > 1 {
				field.State = "uncertain"
			}
		} else {
			field = resolveCandidateSet(input, region.FieldID, field.Candidates, field.Issues, true)
		}
		result.Fields[region.FieldID] = field
	}
	return result, nil
}

func validateVLReader(reader VLReaderDescriptor) error {
	if reader.ModelID != vlModelID || !fieldEvidenceSHA.MatchString(reader.ModelTreeSHA256) || reader.Backend != "native" || reader.Device != "cpu" || reader.PipelineVersion != vlPipelineVersion || reader.UseLayoutDetection || reader.UseDocPreprocessor {
		return errors.New("unsupported VL reader descriptor")
	}
	return nil
}

func validateVLRead(read VLFieldRead) error {
	if read.RegionID == "" || (read.Status != "ok" && read.Status != "empty" && read.Status != "error") || len(read.Blocks) > vlMaxBlocks || math.IsNaN(read.ElapsedSeconds) || math.IsInf(read.ElapsedSeconds, 0) || read.ElapsedSeconds < 0 {
		return errors.New("invalid VL field read")
	}
	if read.Status != "ok" && len(read.Blocks) != 0 {
		return errors.New("non-ok VL read contains text")
	}
	if read.Status == "ok" && (len(read.Blocks) == 0 || read.GeometryScope != vlGeometryScope) {
		return errors.New("invalid successful VL field read")
	}
	if read.Status != "ok" && read.GeometryScope != "" && read.GeometryScope != vlGeometryScope {
		return errors.New("invalid VL geometry scope")
	}
	total := 0
	for _, block := range read.Blocks {
		total += len(block.TextRaw)
		if strings.TrimSpace(block.TextRaw) == "" {
			return errors.New("empty VL text block")
		}
	}
	if total > vlMaxTextBytes {
		return errors.New("VL field text exceeds limit")
	}
	return nil
}

func sameVLRegion(a, b VLFieldRegion) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}

func vlFieldAllowed(field string) bool {
	switch field {
	case "surname", "given_name", "patronymic", "gender", "birth_place", "issuer":
		return true
	default:
		return false
	}
}

func joinVLBlocks(blocks []VLFieldBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.TextRaw)
	}
	return strings.Join(parts, " ")
}

func vlFieldValue(field, raw string, normalizeNames bool) (string, []string, bool) {
	value := cleanValue(stripOwnVLLabel(raw, field))
	if field == "gender" {
		return vlExplicitGender(value)
	}
	if value == "" || hasVLFieldLabel(value) || hasVLCaption(value) || (field == "issuer" && hasVLIssuerLabel(value)) || strings.Contains(value, "<") {
		return "", nil, false
	}
	rules := []string{}
	switch field {
	case "surname", "given_name", "patronymic":
		if !nameLike(value) {
			return "", nil, false
		}
		value, rules, _ = normalizeName(value, normalizeNames)
		if containsLatin(value) || !nameLike(value) {
			return "", nil, false
		}
	case "birth_place":
		if !hasLetters(value) || hasVLNumericHazard(value) {
			return "", nil, false
		}
		value, rules, _ = normalizeName(value, true)
		if containsLatin(value) {
			return "", nil, false
		}
		rules = append(rules, "visual_place_normalization_proposal")
	case "issuer":
		if !hasLetters(value) || hasVLNumericHazard(value) || !hasAnyLabel(value, authorityKeywords) {
			return "", nil, false
		}
		value, rules, _ = normalizeName(value, true)
		if containsLatin(value) {
			return "", nil, false
		}
		rules = append(rules, "visual_issuer_normalization_proposal")
	default:
		return "", nil, false
	}
	return value, rules, true
}

func vlExplicitGender(raw string) (string, []string, bool) {
	if strings.ContainsAny(raw, "0123456789<") {
		return "", nil, false
	}
	for _, r := range raw {
		if !unicode.IsLetter(r) && !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			return "", nil, false
		}
	}
	words := strings.FieldsFunc(raw, func(r rune) bool { return !unicode.IsLetter(r) })
	sexTokens := 0
	sexValue := ""
	allowedLabels := map[string]bool{"ПОЛ": true, "ДАТА": true, "РОЖДЕНИЯ": true, "ДАТАРОЖДЕНИЯ": true}
	for _, word := range words {
		canonical := canonicalLabel(word)
		if allowedLabels[canonical] {
			continue
		}
		gender, ok := canonicalGender(canonical)
		if !ok {
			return "", nil, false
		}
		sexTokens++
		sexValue = gender
	}
	if sexTokens != 1 {
		return "", nil, false
	}
	return sexValue, []string{"vl_explicit_gender_token_in_assigned_row"}, true
}

func hasVLIssuerLabel(value string) bool {
	if strings.HasPrefix(canonicalLabel(value), "ПАСПОРТВЫДАН") {
		return true
	}
	words := strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) })
	for i, word := range words {
		words[i] = canonicalLabel(word)
	}
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "ПАСПОРТ" && words[i+1] == "ВЫДАН" {
			return true
		}
	}
	for _, word := range words {
		if word == "ПАСПОРТВЫДАН" {
			return true
		}
	}
	return false
}

func hasVLCaption(value string) bool {
	words := strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) })
	for i, word := range words {
		words[i] = canonicalLabel(word)
	}
	phrases := [][]string{{"ПАСПОРТ"}, {"РОССИЙСКОЙ", "ФЕДЕРАЦИИ"}, {"РОССИЙСКАЯ", "ФЕДЕРАЦИЯ"}, {"PASSPORT"}, {"RUSSIAN", "FEDERATION"}}
	for _, phrase := range phrases {
		for start := 0; start+len(phrase) <= len(words); start++ {
			matched := true
			for offset, word := range phrase {
				if words[start+offset] != word {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
	}
	return false
}

func hasVLFieldLabel(raw string) bool {
	words := strings.FieldsFunc(raw, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, word := range words {
		words[i] = canonicalLabel(word)
	}
	for _, terms := range labelTerms {
		for _, term := range terms {
			for start := range words {
				joined := ""
				for end := start; end < len(words) && len(joined) <= len(term); end++ {
					joined += words[end]
					if joined == term {
						return true
					}
				}
			}
		}
	}
	return false
}

var vlDeptToken = regexp.MustCompile(`(?:^|[^0-9])\d{3}-\d{3}(?:$|[^0-9])`)
var vlSixDigitToken = regexp.MustCompile(`(?:^|[^0-9])\d{6}(?:$|[^0-9])`)

func hasVLNumericHazard(value string) bool {
	return vlDateToken.MatchString(value) || vlDeptToken.MatchString(value) || vlSixDigitToken.MatchString(value)
}

func stripOwnVLLabel(raw, field string) string {
	terms, ok := labelTerms[field]
	if !ok {
		return raw
	}
	terms = append([]string(nil), terms...)
	if field == "issuer" {
		terms = append(terms, "ПАСПОРТВЫДАН")
	}
	text := strings.TrimSpace(raw)
	for _, term := range terms {
		var canonical strings.Builder
		for offset, r := range text {
			canonical.WriteString(canonicalLabel(string(r)))
			if canonical.String() == term {
				rest := strings.TrimLeft(text[offset+len(string(r)):], " \t:：-—")
				if rest != text[offset+len(string(r)):] {
					return rest
				}
				break
			}
			if !strings.HasPrefix(term, canonical.String()) {
				break
			}
		}
	}
	return text
}

func mergeVLProvenance(a, b []VLFieldProvenance) []VLFieldProvenance {
	for _, source := range b {
		encoded, _ := json.Marshal(source)
		found := false
		for _, existing := range a {
			other, _ := json.Marshal(existing)
			if string(encoded) == string(other) {
				found = true
				break
			}
		}
		if !found {
			a = append(a, source)
		}
	}
	return a
}

func mergeCandidateSets(a, b []Candidate) []Candidate {
	for _, candidate := range b {
		found := false
		for index := range a {
			if a[index].Value == candidate.Value {
				a[index] = mergeCandidate(a[index], candidate)
				found = true
				break
			}
		}
		if !found {
			a = append(a, candidate)
		}
	}
	return a
}

func cloneExtraction(source Extraction) Extraction {
	clone := source
	clone.Fields = make(map[string]FieldResult, len(source.Fields))
	for id, field := range source.Fields {
		candidates := make([]Candidate, len(field.Candidates))
		for i, candidate := range field.Candidates {
			candidates[i] = cloneCandidate(candidate)
		}
		field.Candidates = candidates
		field.Rules = append([]string(nil), field.Rules...)
		field.Issues = append([]string(nil), field.Issues...)
		field.Evidence.LineIDs = append([]string(nil), field.Evidence.LineIDs...)
		field.Evidence.PassIDs = append([]string(nil), field.Evidence.PassIDs...)
		clone.Fields[id] = field
	}
	return clone
}

func cloneCandidate(source Candidate) Candidate {
	clone := source
	clone.RawVariants = append([]string(nil), source.RawVariants...)
	clone.Rules = append([]string(nil), source.Rules...)
	clone.LineIDs = append([]string(nil), source.LineIDs...)
	clone.PassIDs = append([]string(nil), source.PassIDs...)
	clone.FieldEvidence = append([]FieldEvidenceProvenance(nil), source.FieldEvidence...)
	for i := range clone.FieldEvidence {
		clone.FieldEvidence[i].SourcePassIDs = append([]string(nil), source.FieldEvidence[i].SourcePassIDs...)
		clone.FieldEvidence[i].SourceLineIDs = append([]string(nil), source.FieldEvidence[i].SourceLineIDs...)
		clone.FieldEvidence[i].NameRowLineIDs = cloneLineIDs(source.FieldEvidence[i].NameRowLineIDs)
		clone.FieldEvidence[i].AdjacentAnchors = append([]FieldEvidenceAnchor(nil), source.FieldEvidence[i].AdjacentAnchors...)
	}
	clone.TemplateEvidence = append([]TemplateProvenance(nil), source.TemplateEvidence...)
	for i := range clone.TemplateEvidence {
		clone.TemplateEvidence[i].Anchors = cloneStringMap(source.TemplateEvidence[i].Anchors)
		clone.TemplateEvidence[i].Region.AnchorIDs = append([]string(nil), source.TemplateEvidence[i].Region.AnchorIDs...)
		clone.TemplateEvidence[i].Region.Records = append([]TemplateRecord(nil), source.TemplateEvidence[i].Region.Records...)
		clone.TemplateEvidence[i].Region.Alternatives = make([][]TemplateRecord, len(source.TemplateEvidence[i].Region.Alternatives))
		for j := range source.TemplateEvidence[i].Region.Alternatives {
			clone.TemplateEvidence[i].Region.Alternatives[j] = append([]TemplateRecord(nil), source.TemplateEvidence[i].Region.Alternatives[j]...)
		}
	}
	clone.VLEvidence = append([]VLFieldProvenance(nil), source.VLEvidence...)
	for i := range clone.VLEvidence {
		clone.VLEvidence[i].Region.AnchorLineIDs = append([]string(nil), source.VLEvidence[i].Region.AnchorLineIDs...)
		clone.VLEvidence[i].Read.Region.AnchorLineIDs = append([]string(nil), source.VLEvidence[i].Read.Region.AnchorLineIDs...)
		clone.VLEvidence[i].Read.Blocks = append([]VLFieldBlock(nil), source.VLEvidence[i].Read.Blocks...)
		if source.VLEvidence[i].Read.CapReached != nil {
			capReached := *source.VLEvidence[i].Read.CapReached
			clone.VLEvidence[i].Read.CapReached = &capReached
		}
	}
	return clone
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
