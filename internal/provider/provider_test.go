package provider

import (
	"slices"
	"testing"
)

func TestClassify(t *testing.T) {
	c := New(Default)
	tests := []struct{ domain, want string }{
		{"gmail.com", "gmail"},
		{"GMAIL.COM.", "gmail"},
		{"mail.google.com", "gmail"},
		{"google.com", "gmail"},
		{"hotmail.co.uk", "outlook"},
		{"att.net", "yahoo"},
		{"mail.icloud.com", "icloud"},
		{"yahoo.com.br", "yahoo"},
		{"wanadoo.fr", "orange"},
		{"web.de", "gmx"},
		{"nyc.rr.com", "spectrum"},
		{"126.com", "netease"},
		{"bol.com.br", "uol"},
		{"hotmail.com", "outlook"},
		{"eu.live.com", "outlook"},
		{"aol.com", "yahoo"},
		{"example.org", Other},
		{"", Other},
		{"notgmail.com", Other},
	}
	for _, tc := range tests {
		if got := c.Classify(tc.domain); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

func TestCustomRulesAndNames(t *testing.T) {
	c := New([]Rule{
		{Name: "Corp", Domains: []string{".corp.example", "mail.example"}},
		{Name: "corp", Domains: []string{"other.example"}},
		{Name: "", Domains: []string{"ignored.example"}},
	})
	if got := c.Classify("a.b.corp.example"); got != "corp" {
		t.Errorf("suffix match = %q, want corp", got)
	}
	if got := c.Classify("ignored.example"); got != Other {
		t.Errorf("rule without a name classified as %q", got)
	}
	if got, want := c.Names(), []string{"corp", Other}; !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestLongestSuffixWins(t *testing.T) {
	c := New([]Rule{
		{Name: "a", Domains: []string{".example.com"}},
		{Name: "b", Domains: []string{".mail.example.com"}},
	})
	if got := c.Classify("x.mail.example.com"); got != "b" {
		t.Errorf("Classify = %q, want the longer suffix b", got)
	}
}

func TestLaterRuleWinsForTheSameDomainOrSuffix(t *testing.T) {
	c := New([]Rule{
		{Name: "a", Domains: []string{"x.example", ".y.example"}},
		{Name: "b", Domains: []string{"x.example", ".y.example"}},
	})
	for _, d := range []string{"x.example", "m.y.example", "y.example"} {
		if got := c.Classify(d); got != "b" {
			t.Errorf("Classify(%q) = %q, want the later rule b", d, got)
		}
	}
}
