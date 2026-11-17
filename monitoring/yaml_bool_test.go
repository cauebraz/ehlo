package monitoring

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestYAML11BoolWordsAreQuoted(t *testing.T) {
	for _, s := range []string{"y", "Y", "yes", "Yes", "n", "no", "NO", "on", "On", "off", "OFF", "true", "false"} {
		b, err := yaml.Marshal(str(s))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(b)); got != `"`+s+`"` {
			t.Errorf("str(%q) marshals as %s, want it double quoted so YAML 1.1 readers keep a string", s, got)
		}
	}
	b, err := yaml.Marshal(str("line"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "line" {
		t.Errorf("str(%q) marshals as %s, want it plain", "line", got)
	}
}
