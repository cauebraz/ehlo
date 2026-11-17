// Package config reads the ehlo server configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// Config is the server configuration file. Load decodes it over Default,
// so a file names only what it changes.
type Config struct {
	Listen    string        `yaml:"listen"`
	StatePath string        `yaml:"state_path"`
	Snapshot  time.Duration `yaml:"snapshot_interval"`
	// Providers maps recipient domains to providers. Set, it replaces the
	// built-in list.
	Providers []provider.Rule `yaml:"providers"`
	// ProvidersExtra adds rules after Providers. For a domain or suffix
	// both list, ProvidersExtra wins.
	ProvidersExtra []provider.Rule `yaml:"providers_extra"`
	Warmup         warmup.Policy   `yaml:"warmup"`
	Targets        warmup.Targets  `yaml:"targets"`
	// Sources primes zero-valued series for known sources, so a source that
	// never sends shows 0 instead of no data.
	Sources []string `yaml:"sources"`
	// MaxBodyBytes bounds one ingest request.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// Routes lists routes accepted beyond self_hosted, ses and sendgrid.
	Routes []string `yaml:"routes"`
	// MaxSources bounds the distinct sources ingest accepts, so a client
	// cannot create unbounded series and lanes.
	MaxSources int `yaml:"max_sources"`
	// Webhooks authenticates the ESP webhook endpoints.
	Webhooks Webhooks `yaml:"webhooks"`
}

// Webhooks holds the credentials the ESP webhook endpoints check.
type Webhooks struct {
	// Token, when set, must be sent as the token query parameter on every
	// webhook request.
	Token string `yaml:"token"`
	// SendGridPublicKey, when set, is the base64 DER ECDSA public key from
	// SendGrid's Signed Event Webhook settings, and unsigned requests fail.
	SendGridPublicKey string `yaml:"sendgrid_public_key"`
	// TokenFile names a file whose trimmed content is Token, such as a
	// mounted Kubernetes Secret key. It excludes Token.
	TokenFile string `yaml:"token_file"`
	// SendGridPublicKeyFile names a file whose trimmed content is
	// SendGridPublicKey. It excludes SendGridPublicKey.
	SendGridPublicKeyFile string `yaml:"sendgrid_public_key_file"`
}

// readSecrets fills Token and SendGridPublicKey from their files, once at
// load. A file set next to its inline key, unreadable or empty is an error.
func (w *Webhooks) readSecrets() error {
	var errs []error
	read := func(key, path string, dst *string) {
		if path == "" {
			return
		}
		if *dst != "" {
			errs = append(errs, fmt.Errorf("webhooks.%s and webhooks.%s_file are both set", key, key))
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("webhooks.%s_file: %w", key, err))
			return
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			errs = append(errs, fmt.Errorf("webhooks.%s_file: %s is empty", key, path))
			return
		}
		*dst = v
	}
	read("token", w.TokenFile, &w.Token)
	read("sendgrid_public_key", w.SendGridPublicKeyFile, &w.SendGridPublicKey)
	return errors.Join(errs...)
}

// DefaultMaxBodyBytes bounds one ingest request.
const DefaultMaxBodyBytes = 4 << 20

// DefaultMaxSources bounds the distinct sources ingest accepts.
const DefaultMaxSources = 256

// Default is the configuration ehlo runs with when no file is given. Each
// call returns a fresh copy.
func Default() Config {
	return Config{
		Listen:       ":8080",
		StatePath:    "data/warmup.json",
		Snapshot:     30 * time.Second,
		Providers:    slices.Clone(provider.Default),
		Warmup:       warmup.DefaultPolicy(),
		MaxBodyBytes: DefaultMaxBodyBytes,
		MaxSources:   DefaultMaxSources,
	}
}

// Load reads a YAML file over Default. An empty path returns Default.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, c.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, errors.Join(c.Webhooks.readSecrets(), c.Validate())
}

// Validate returns every problem in the configuration at once.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.Snapshot <= 0 {
		errs = append(errs, errors.New("snapshot_interval must be positive"))
	}
	if c.StatePath == "" {
		errs = append(errs, errors.New("state_path is required"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("max_body_bytes must be positive"))
	}
	if c.MaxSources < 1 {
		errs = append(errs, errors.New("max_sources must be at least 1"))
	}
	if n := len(c.Sources); n > c.MaxSources {
		errs = append(errs, fmt.Errorf("sources lists %d names, above max_sources %d", n, c.MaxSources))
	}
	errs = append(errs, validRules("providers", c.Providers)...)
	errs = append(errs, validRules("providers_extra", c.ProvidersExtra)...)
	errs = append(errs, c.unknownProviders()...)
	if err := c.Warmup.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("warmup: %w", err))
	}
	if err := c.Targets.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("targets: %w", err))
	}
	return errors.Join(errs...)
}

// ProviderRules returns Providers followed by ProvidersExtra, the rules the
// classifier compiles.
func (c Config) ProviderRules() []provider.Rule {
	return slices.Concat(c.Providers, c.ProvidersExtra)
}

var (
	providerName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	domainRule   = regexp.MustCompile(`^\.?[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*$`)
)

// validRules checks that every rule has a label-safe name and at least one
// domain or suffix, each a dotted host name with an optional leading dot.
func validRules(section string, rules []provider.Rule) []error {
	var errs []error
	for i, r := range rules {
		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("%s[%d]: name is required", section, i))
		case !providerName.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s[%d]: name %q must be 1-32 letters, digits, '.', '_' or '-'", section, i, r.Name))
		}
		if len(r.Domains) == 0 {
			errs = append(errs, fmt.Errorf("%s[%d]: domains is empty", section, i))
		}
		for j, d := range r.Domains {
			if len(d) > 253 || !domainRule.MatchString(d) {
				errs = append(errs, fmt.Errorf("%s[%d].domains[%d]: %q is not a domain or a .suffix", section, i, j, d))
			}
		}
	}
	return errs
}

// unknownProviders reports per-provider keys that name no provider, so a
// typo such as gmial does not silently fall back to the default.
func (c Config) unknownProviders() []error {
	known := map[string]bool{warmup.DefaultKey: true, provider.Other: true}
	for _, n := range provider.New(c.ProviderRules()).Names() {
		known[n] = true
	}
	var errs []error
	check := func(section string, keys []string) {
		slices.Sort(keys)
		for _, k := range keys {
			if !known[k] {
				errs = append(errs, fmt.Errorf("%s.%s: unknown provider", section, k))
			}
		}
	}
	check("warmup.growth", slices.Collect(maps.Keys(c.Warmup.Growth)))
	check("warmup.shaping", slices.Collect(maps.Keys(c.Warmup.Shaping)))
	check("targets.mix", slices.Collect(maps.Keys(c.Targets.Mix)))
	return errs
}
