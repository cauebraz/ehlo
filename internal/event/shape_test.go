package event

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBoundsSourceAndRoute(t *testing.T) {
	for _, tc := range []struct {
		source, route string
		ok            bool
	}{
		{"mx-1", "", true},
		{"pool.eu-1:a_b", "self_hosted", true},
		{"has space", "", false},
		{strings.Repeat("a", 65), "", false},
		{"mx-1", "bad/route", false},
	} {
		e := Event{Type: Complaint, Time: now, Source: tc.source, Route: tc.route}
		err := e.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("source %q route %q: err = %v, want ok=%v", tc.source, tc.route, err, tc.ok)
		}
	}
}
