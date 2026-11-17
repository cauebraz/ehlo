package main

import (
	"slices"
	"strings"
	"testing"
)

func TestRunRejectsBadFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"-bogus"}, "bogus"},
		{"unknown scenario", []string{"-scenario", "nope"}, "unknown scenario"},
		{"zero speed", []string{"-speed", "0"}, "must be positive"},
		{"negative tick", []string{"-tick", "-1s"}, "must be positive"},
		{"no sources", []string{"-sources", " , "}, "at least one source"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run(%v) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"mx-1", []string{"mx-1"}},
		{" mx-1 ,mx-2,, ", []string{"mx-1", "mx-2"}},
	}
	for _, tc := range tests {
		if got := split(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("split(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
