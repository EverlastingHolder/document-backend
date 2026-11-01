package postgres

import (
	"strings"
	"testing"

	"passport-backend/internal/jobs"
)

func TestValidSubmission(t *testing.T) {
	valid := jobs.Submission{
		JobID:          "4dd683a3-9efa-4b31-9b5b-8ae58c0e2ca1",
		EventID:        "591d1d58-d38e-4ef8-8cf0-68f9b35d0c31",
		OwnerID:        "owner-opaque",
		IdempotencyKey: "request-key_1",
		ImageKey:       "a57f3945-08cd-441b-85d5-7962281034be",
		ImageSHA256:    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		MediaType:      "image/jpeg",
		Size:           42,
	}
	if !validSubmission(valid) {
		t.Fatal("validSubmission() rejected a valid UUIDv4-backed image job")
	}

	tests := []struct {
		name   string
		mutate func(*jobs.Submission)
	}{
		{name: "job ID is not a UUIDv4", mutate: func(value *jobs.Submission) { value.JobID = "bad" }},
		{name: "event ID is not a UUIDv4", mutate: func(value *jobs.Submission) { value.EventID = "bad" }},
		{name: "image key is not a UUIDv4", mutate: func(value *jobs.Submission) { value.ImageKey = "bad" }},
		{name: "missing owner", mutate: func(value *jobs.Submission) { value.OwnerID = "" }},
		{name: "invalid idempotency key", mutate: func(value *jobs.Submission) { value.IdempotencyKey = "has space" }},
		{name: "unsupported media type", mutate: func(value *jobs.Submission) { value.MediaType = "image/gif" }},
		{name: "nonpositive size", mutate: func(value *jobs.Submission) { value.Size = 0 }},
		{name: "malformed image digest", mutate: func(value *jobs.Submission) { value.ImageSHA256 = "not-a-digest" }},
		{name: "uppercase image digest", mutate: func(value *jobs.Submission) { value.ImageSHA256 = strings.Repeat("A", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			if validSubmission(input) {
				t.Fatal("validSubmission() accepted invalid metadata")
			}
		})
	}
}
