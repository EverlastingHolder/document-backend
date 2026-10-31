package passport

const ExtractionSchema = "passport-extraction/v1"

type Extraction struct {
	SchemaVersion  string                 `json:"schema_version"`
	ImageID        string                 `json:"image_id"`
	ReviewRequired bool                   `json:"review_required"`
	Fields         map[string]FieldResult `json:"fields"`
}

type FieldResult struct {
	Value      *string     `json:"value"`
	State      string      `json:"state"`
	Candidates []Candidate `json:"candidates"`
	Issues     []string    `json:"issues"`
}

type Candidate struct {
	Value string `json:"value"`
}

func emptyField() FieldResult {
	return FieldResult{
		State:      "missing",
		Candidates: []Candidate{},
		Issues:     []string{},
	}
}
