package passport

import (
	"encoding/json"
	"testing"
)

func TestConflictingEvidenceOrderIsStableAcrossRepeatedResolutions(t *testing.T) {
	input := syntheticObservations()
	candidates := []Candidate{
		{Value: "А", Raw: "raw-a", RawVariants: []string{"raw-a"}, Rules: []string{"rule-a"}, LineIDs: []string{"line-z", "line-a"}, PassIDs: []string{"pass-z"}, Context: "source-a"},
		{Value: "Б", Raw: "raw-b", RawVariants: []string{"raw-b"}, Rules: []string{"rule-b"}, LineIDs: []string{"line-b"}, PassIDs: []string{"pass-b", "pass-a"}, Context: "source-b"},
	}
	var first []byte
	for i := 0; i < 100; i++ {
		result := resolveCandidateSet(input, "given_name", candidates, nil, false)
		if result.Value != nil || result.State != "uncertain" || len(result.Candidates) != 2 {
			t.Fatalf("conflicting values changed selection: state=%s valuePresent=%t candidates=%d", result.State, result.Value != nil, len(result.Candidates))
		}
		if got, want := result.Evidence.LineIDs, []string{"line-z", "line-a", "line-b"}; !equalStrings(got, want) {
			t.Fatalf("line evidence order = %v, want %v", got, want)
		}
		if got, want := result.Evidence.PassIDs, []string{"pass-z", "pass-b", "pass-a"}; !equalStrings(got, want) {
			t.Fatalf("pass evidence order = %v, want %v", got, want)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = encoded
			continue
		}
		if string(encoded) != string(first) {
			t.Fatalf("resolution JSON changed on iteration %d", i)
		}
	}
}

func TestExtractAndVLPlanBaselineSHAIsStableAcrossRepeatedRuns(t *testing.T) {
	input, template := templateFixture()
	opts := Options{PassSet: "all", NormalizeNames: true}
	var first string
	for i := 0; i < 100; i++ {
		baseline, err := Extract(input, opts)
		if err != nil {
			t.Fatal(err)
		}
		baseline, err = ApplyTemplateEvidence(input, opts, baseline, &template)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := BuildVLFieldPlan(input, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = plan.BaselineSHA256
			continue
		}
		if plan.BaselineSHA256 != first {
			t.Fatalf("baseline hash changed on iteration %d", i)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
