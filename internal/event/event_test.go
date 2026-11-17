package event

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		e    Event
		ok   bool
	}{
		{"attempt", Event{Type: Attempt, Time: now, Source: "a"}, true},
		{"attempt bad kind", Event{Type: Attempt, Time: now, Source: "a", Attempt: "third"}, false},
		{"reply 250", Event{Type: Reply, Time: now, Source: "a", Code: 250, Enhanced: "2.0.0"}, true},
		{"reply connect without code", Event{Type: Reply, Time: now, Source: "a", Stage: StageConnect}, true},
		{"reply unknown stage", Event{Type: Reply, Time: now, Source: "a", Code: 421, Stage: "bogus"}, false},
		{"reply rcpt stage", Event{Type: Reply, Time: now, Source: "a", Code: 421, Stage: StageRcpt}, true},
		{"reply code 199", Event{Type: Reply, Time: now, Source: "a", Code: 199}, false},
		{"reply code 354", Event{Type: Reply, Time: now, Source: "a", Code: 354}, false},
		{"reply code 399", Event{Type: Reply, Time: now, Source: "a", Code: 399}, false},
		{"reply code 599", Event{Type: Reply, Time: now, Source: "a", Code: 599}, true},
		{"reply bad enhanced", Event{Type: Reply, Time: now, Source: "a", Code: 421, Enhanced: "4.7"}, false},
		{"reply enhanced long", Event{Type: Reply, Time: now, Source: "a", Code: 421, Enhanced: "4.7.1234"}, false},
		{"bounce hard", Event{Type: Bounce, Time: now, Source: "a", Bounce: Hard}, true},
		{"bounce unknown", Event{Type: Bounce, Time: now, Source: "a", Bounce: "weird"}, false},
		{"queue", Event{Type: Queue, Time: now, Source: "a", Deferred: 10, OldestAge: time.Hour}, true},
		{"queue negative", Event{Type: Queue, Time: now, Source: "a", Deferred: -1}, false},
		{"dmarc", Event{Type: DMARC, Time: now, Source: "a", SPF: Pass, DKIM: Fail}, true},
		{"dmarc missing", Event{Type: DMARC, Time: now, Source: "a", SPF: Pass}, false},
		{"no source", Event{Type: Open, Time: now}, false},
		{"no time", Event{Type: Open, Source: "a"}, false},
		{"wrong version", Event{Version: 2, Type: Open, Time: now, Source: "a"}, false},
		{"negative count", Event{Type: Open, Time: now, Source: "a", Count: -3}, false},
		{"unknown type", Event{Type: "nope", Time: now, Source: "a"}, false},
		{"reply with tag and delay", Event{Type: Reply, Time: now, Source: "a", Code: 421, Enhanced: "4.7.0", Tag: "TSS04", Delay: 1.5, Text: "x"}, true},
		{"reply bad tag", Event{Type: Reply, Time: now, Source: "a", Code: 421, Tag: "hello world"}, false},
		{"reply provider tags", Event{Type: Reply, Time: now, Source: "a", Code: 554, Tag: "AUP#IN-1310"}, true},
		{"reply tag too long", Event{Type: Reply, Time: now, Source: "a", Code: 554, Tag: "OFR004_104_AND_MORE_TEXT_HERE"}, false},
		{"reply negative delay", Event{Type: Reply, Time: now, Source: "a", Code: 250, Delay: -1}, false},
		{"reply long text", Event{Type: Reply, Time: now, Source: "a", Code: 250, Text: string(make([]byte, MaxTextLength+1))}, false},
		{"tls", Event{Type: TLS, Time: now, Source: "a", TLSVersion: "TLSv1.3", TLSTrust: "untrusted"}, true},
		{"tls bad version", Event{Type: TLS, Time: now, Source: "a", TLSVersion: "SSLv3", TLSTrust: "untrusted"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestClassAndLabel(t *testing.T) {
	tests := []struct {
		e          Event
		class, lbl string
	}{
		{Event{Code: 250, Enhanced: "2.0.0"}, "2xx", "250 2.0.0"},
		{Event{Code: 421, Enhanced: "4.7.28"}, "4xx", "421 4.7.28"},
		{Event{Code: 550}, "5xx", "550"},
		{Event{Stage: StageTLS}, "connect", "tls"},
		{Event{Stage: StageRcpt, Code: 450, Enhanced: "4.2.1"}, "4xx", "450 4.2.1"},
		{Event{Code: 0}, "other", "0"},
	}
	for _, tc := range tests {
		if got := tc.e.Class(); got != Class(tc.class) {
			t.Errorf("%+v Class() = %q, want %q", tc.e, got, tc.class)
		}
		if got := tc.e.CodeLabel(); got != tc.lbl {
			t.Errorf("%+v CodeLabel() = %q, want %q", tc.e, got, tc.lbl)
		}
	}
}

func TestN(t *testing.T) {
	if got := (&Event{}).N(); got != 1 {
		t.Errorf("N() of zero count = %d, want 1", got)
	}
	if got := (&Event{Count: 7}).N(); got != 7 {
		t.Errorf("N() = %d, want 7", got)
	}
}

func TestCodeLabelWithTag(t *testing.T) {
	e := Event{Code: 421, Enhanced: "4.7.0", Tag: "TSS04"}
	if got := e.CodeLabelWithTag(); got != "421 4.7.0 TSS04" {
		t.Errorf("CodeLabelWithTag() = %q", got)
	}
	c := Event{Stage: StageConnect, Tag: "S775"}
	if got := c.CodeLabelWithTag(); got != "connect" {
		t.Errorf("connect label = %q, want connect", got)
	}
}
