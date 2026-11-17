package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cauebraz/ehlo/internal/provider"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ehlo.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Warmup.StartLimit != 50 || len(c.Providers) == 0 {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoadEmptyFileKeepsDefaults(t *testing.T) {
	for _, body := range []string{"", "# only a comment\n"} {
		c, err := Load(write(t, body))
		if err != nil {
			t.Errorf("load %q: %v, want the defaults", body, err)
			continue
		}
		if c.Listen != ":8080" || c.Warmup.StartLimit != 50 {
			t.Errorf("load %q = %+v, want the defaults", body, c)
		}
	}
}

func TestLoadOverridesAndKeepsDefaults(t *testing.T) {
	p := write(t, `
listen: ":9000"
warmup:
  start_limit: 100
  growth:
    gmail: 1.5
targets:
  default: 180000
  mix:
    gmail: 0.55
    outlook: 0.2
  sources:
    mx-1: {daily: 200000, warm: true}
sources: [mx-1, mx-2]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.Warmup.StartLimit != 100 {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.Warmup.Growth["gmail"] != 1.5 || c.Warmup.Growth["default"] != 1.4 {
		t.Errorf("growth = %v, want gmail 1.5 with default 1.4 kept", c.Warmup.Growth)
	}
	if c.Warmup.HoldComplaintRatio != 0.001 {
		t.Errorf("hold ratio lost its default: %v", c.Warmup.HoldComplaintRatio)
	}
	if c.Targets.Sources["mx-1"].Daily != 200000 || !c.Targets.Sources["mx-1"].Warm {
		t.Errorf("source target = %+v", c.Targets.Sources["mx-1"])
	}
	if c.Snapshot.Seconds() != 30 {
		t.Errorf("snapshot interval = %v, want 30s", c.Snapshot)
	}
}

func TestLoadRejectsUnknownFieldsAndBadValues(t *testing.T) {
	if _, err := Load(write(t, "listne: x\n")); err == nil || !strings.Contains(err.Error(), "listne") {
		t.Errorf("unknown field accepted: %v", err)
	}
	if _, err := Load(write(t, "warmup:\n  decision_hour: 30\n")); err == nil {
		t.Error("decision_hour 30 accepted")
	}
	if _, err := Load(write(t, "targets:\n  mix:\n    a: 0.8\n    b: 0.8\n")); err == nil {
		t.Error("mix above 1 accepted")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestValidateRejectsUnknownProviderKeys(t *testing.T) {
	p := write(t, `
targets:
  mix: {gmial: 0.5}
warmup:
  growth: {default: 1.4, yahooo: 1.3}
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "targets.mix.gmial") || !strings.Contains(err.Error(), "warmup.growth.yahooo") {
		t.Errorf("err = %v, want both typos reported", err)
	}
}

func TestValidateRequiresStatePathAndBoundsSources(t *testing.T) {
	p := write(t, `
state_path: ""
max_sources: 1
sources: [mx-1, mx-2]
targets:
  sources:
    mx-3: {daily: -1}
`)
	_, err := Load(p)
	for _, want := range []string{"state_path is required", "above max_sources 1", "targets.sources.mx-3.daily"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestProvidersExtraExtendsTheBuiltInList(t *testing.T) {
	c, err := Load(write(t, `
providers_extra:
  - name: gmail
    domains: [workspace.example]
  - name: corp
    domains: [corp.example, .corp.example]
  - name: aol
    domains: [aol.com]
targets:
  mix: {corp: 0.1}
`))
	if err != nil {
		t.Fatal(err)
	}
	cls := provider.New(c.ProviderRules())
	for domain, want := range map[string]string{
		"gmail.com":         "gmail",
		"workspace.example": "gmail",
		"mx.corp.example":   "corp",
		"aol.com":           "aol",
		"yahoo.com":         "yahoo",
		"unlisted.example":  provider.Other,
	} {
		if got := cls.Classify(domain); got != want {
			t.Errorf("Classify(%q) = %q, want %q", domain, got, want)
		}
	}
}

func TestProvidersExtraAppliesAfterAReplacedList(t *testing.T) {
	c, err := Load(write(t, `
providers:
  - name: gmail
    domains: [gmail.com]
  - name: outlook
    domains: [outlook.com]
  - name: yahoo
    domains: [yahoo.co.jp]
  - name: icloud
    domains: [icloud.com]
providers_extra:
  - name: corp
    domains: [corp.example]
`))
	if err != nil {
		t.Fatal(err)
	}
	cls := provider.New(c.ProviderRules())
	if got := cls.Classify("yahoo.com"); got != provider.Other {
		t.Errorf("yahoo.com = %q, want other once providers replaced the list", got)
	}
	if got := cls.Classify("corp.example"); got != "corp" {
		t.Errorf("corp.example = %q, want corp", got)
	}
}

func TestProviderRulesAreValidated(t *testing.T) {
	_, err := Load(write(t, `
providers_extra:
  - name: ""
    domains: [a.example]
  - name: "bad name"
    domains: [b.example]
  - name: empty
    domains: []
  - name: badhost
    domains: [user@c.example, "", "d example"]
`))
	for _, want := range []string{
		"providers_extra[0]: name is required",
		`providers_extra[1]: name "bad name"`,
		"providers_extra[2]: domains is empty",
		`providers_extra[3].domains[0]: "user@c.example"`,
		`providers_extra[3].domains[1]: ""`,
		`providers_extra[3].domains[2]: "d example"`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestWebhookSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(tokenPath, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("MFkwEwYHKoZIzj0CAQ==\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(write(t, "webhooks:\n  token_file: "+tokenPath+"\n  sendgrid_public_key_file: "+keyPath+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Webhooks.Token != "s3cret" || c.Webhooks.SendGridPublicKey != "MFkwEwYHKoZIzj0CAQ==" {
		t.Errorf("webhooks = %+v, want trimmed file contents", c.Webhooks)
	}
}

func TestWebhookSecretFileErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"webhooks:\n  token: a\n  token_file: " + empty + "\n":                             "webhooks.token and webhooks.token_file are both set",
		"webhooks:\n  sendgrid_public_key: a\n  sendgrid_public_key_file: " + empty + "\n": "webhooks.sendgrid_public_key and webhooks.sendgrid_public_key_file are both set",
		"webhooks:\n  token_file: " + empty + "\n":                                         "webhooks.token_file: " + empty + " is empty",
		"webhooks:\n  sendgrid_public_key_file: " + filepath.Join(dir, "missing") + "\n":   "webhooks.sendgrid_public_key_file: open",
	}
	for body, want := range cases {
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err = %v, want %q", body, err, want)
		}
	}
}
