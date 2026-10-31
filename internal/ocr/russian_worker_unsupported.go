//go:build !darwin && !linux

package ocr

import "passport-backend/internal/httpapi"

func newRussianRecognizer(Config) (*Recognizer, error) {
	return nil, httpapi.ErrUnavailable
}
