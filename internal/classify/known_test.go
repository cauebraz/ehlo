package classify_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
)

func TestKnownCodesAreReplyLabels(t *testing.T) {
	if len(classify.KnownCodes) == 0 {
		t.Fatal("KnownCodes is empty")
	}
	seen := map[string]bool{}
	for _, label := range classify.KnownCodes {
		if seen[label] {
			t.Errorf("%q listed twice", label)
		}
		seen[label] = true
		f := strings.Fields(label)
		code, err := strconv.Atoi(f[0])
		if err != nil {
			t.Errorf("%q: code %q is not a number", label, f[0])
			continue
		}
		e := event.Event{Type: event.Reply, Time: time.Unix(0, 0), Source: "mx-1", Code: code}
		for _, part := range f[1:] {
			if strings.Contains(part, ".") {
				e.Enhanced = part
			} else {
				e.Tag = part
			}
		}
		if err := e.Validate(); err != nil {
			t.Errorf("%q: %v", label, err)
		}
		if got := e.CodeLabelWithTag(); got != label {
			t.Errorf("%q: an event with these fields has label %q", label, got)
		}
	}
}
