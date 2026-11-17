package classify

import (
	"strings"
	"testing"

	"github.com/cauebraz/ehlo/internal/event"
)

func TestTagNeverExceedsTheEventShape(t *testing.T) {
	for _, text := range []string{
		"rejected GU_EIB_" + strings.Repeat("1", 30),
		"Relay access denied SFR_IN_" + strings.Repeat("9", 20),
	} {
		if got := Tag(text); got != "" && !event.ValidTag(got) {
			t.Errorf("Tag(%q) = %q, which event validation rejects", text, got)
		}
	}
}

func FuzzTag(f *testing.F) {
	for _, s := range []string{"[TSS04] deferred", "(S3150)", "BL000001", "OFR004_104", "AUP#In-1310", "HL:ICC", "GU_EIB_1", "(BL)"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if got := Tag(text); got != "" && !event.ValidTag(got) {
			t.Errorf("Tag(%q) = %q, which event validation rejects", text, got)
		}
		_ = Classify(Reply{Code: 451, Tag: Tag(text), Text: text})
	})
}
