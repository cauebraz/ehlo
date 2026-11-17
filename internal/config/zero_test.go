package config

import (
	"strings"
	"testing"
)

func TestLoadKeepsExplicitZeroes(t *testing.T) {
	c, err := Load(write(t, "warmup: {idle_days_reset: 0, grow_when_used: 0, growth: {gmail: 2}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Warmup.IdleDaysReset != 0 {
		t.Errorf("idle_days_reset = %d, want the explicit 0 kept over the default 30", c.Warmup.IdleDaysReset)
	}
	if c.Warmup.GrowWhenUsed != 0 {
		t.Errorf("grow_when_used = %v, want the explicit 0 kept over the default 0.7", c.Warmup.GrowWhenUsed)
	}
	if c.Warmup.Growth["gmail"] != 2 || c.Warmup.Growth["default"] != 1.4 || c.Warmup.Growth["yahoo"] != 1.35 {
		t.Errorf("growth = %v, want gmail 2 merged over the defaults", c.Warmup.Growth)
	}
	if c.Warmup.StartLimit != 50 || c.Warmup.PauseDays != 1 {
		t.Errorf("untouched warmup fields lost their defaults: %+v", c.Warmup)
	}
}

func TestLoadRejectsUnknownWarmupKey(t *testing.T) {
	_, err := Load(write(t, "warmup: {idle_days: 0}\n"))
	if err == nil || !strings.Contains(err.Error(), "idle_days") {
		t.Errorf("error = %v, want the unknown key named", err)
	}
}

func TestLoadGrowthWithoutDefaultValidates(t *testing.T) {
	c, err := Load(write(t, "warmup:\n  growth:\n    gmail: 2\n"))
	if err != nil {
		t.Fatalf("Default() supplies growth.default, Load failed: %v", err)
	}
	if err := c.Warmup.Validate(); err != nil {
		t.Errorf("policy invalid: %v", err)
	}
	p := c.Warmup
	p.Growth = map[string]float64{"gmail": 2}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "growth.default is required") {
		t.Errorf("without the default the policy must fail: %v", err)
	}
}
