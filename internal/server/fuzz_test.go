package server

import (
	"bytes"
	"testing"
)

func FuzzDecodeEvents(f *testing.F) {
	for _, seed := range []string{
		`{"type":"attempt","time":"2026-03-10T08:15:02Z","source":"mx-1","domain":"gmail.com"}`,
		`[{"type":"complaint","time":"2026-03-10T09:00:00Z","source":"mx-1"}]`,
		"{\"type\":\"open\"}\n{\"type\":\"open\"}\n",
		"",
		"[",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		events, err := decodeEvents(bytes.NewReader(body))
		if err != nil {
			return
		}
		for i := range events {
			_ = events[i].Validate()
		}
	})
}
