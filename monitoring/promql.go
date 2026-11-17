package monitoring

import (
	"regexp"
	"slices"
	"strings"
)

// alertsName is the alert state series as the dashboard code writes it;
// rendering replaces it with Config.AlertsMetric.
const alertsName = "ALERTS"

var (
	// selectorRe matches a series name with its optional label matchers:
	// raw ehlo_ series, recorded ehlo: series and the alert state series.
	selectorRe = regexp.MustCompile(`\b(ehlo_[a-z0-9_]+|ehlo:[a-z0-9_:]+|` + alertsName + `)(\{[^}]*\})?`)
	// groupingRe matches a by or on clause with its label list.
	groupingRe = regexp.MustCompile(`\b(by|on) \(([^)]*)\)`)
)

// matchers are label matchers added to series selectors, per kind of
// series. An empty list leaves the selector unchanged.
type matchers struct {
	raw, recorded, alerts []string
	alertsMetric          string
}

// inject adds the matchers to every series selector of expr and renames
// the alert state series.
func (m matchers) inject(expr string) string {
	return selectorRe.ReplaceAllStringFunc(expr, func(s string) string {
		sub := selectorRe.FindStringSubmatch(s)
		name, braces := sub[1], sub[2]
		var add []string
		switch {
		case name == alertsName:
			add = m.alerts
			if m.alertsMetric != "" {
				name = m.alertsMetric
			}
		case strings.HasPrefix(name, "ehlo:"):
			add = m.recorded
		default:
			add = m.raw
		}
		if len(add) == 0 {
			return name + braces
		}
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(braces, "{"), "}"))
		parts := []string{}
		if inner != "" {
			parts = append(parts, strings.TrimSuffix(inner, ","))
		}
		return name + "{" + strings.Join(append(parts, add...), ", ") + "}"
	})
}

// group adds labels to the front of every by and on clause of expr that
// lacks them.
func group(expr string, labels []string) string {
	if len(labels) == 0 {
		return expr
	}
	return groupingRe.ReplaceAllStringFunc(expr, func(s string) string {
		sub := groupingRe.FindStringSubmatch(s)
		var have []string
		for _, l := range strings.Split(sub[2], ",") {
			if l = strings.TrimSpace(l); l != "" {
				have = append(have, l)
			}
		}
		var front []string
		for _, l := range labels {
			if !slices.Contains(have, l) {
				front = append(front, l)
			}
		}
		return sub[1] + " (" + strings.Join(append(front, have...), ", ") + ")"
	})
}

// variableMatchers are the matchers of the dashboard filter variables of
// the group_by labels, such as cluster=~"$cluster".
func variableMatchers(labels []string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l + `=~"$` + l + `"`
	}
	return out
}

// selectorMatchers splits a Config selector into its matchers.
func selectorMatchers(sel string) []string {
	sel = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sel), ","))
	if sel == "" {
		return nil
	}
	return oneMatcherRe.FindAllString(sel, -1)
}

// oneMatcherRe matches one label matcher with a double-quoted value.
var oneMatcherRe = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*\s*(?:=~|!~|!=|=)\s*"(?:[^"\\]|\\.)*"`)
