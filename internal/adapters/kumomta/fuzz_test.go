package kumomta

import (
	"os"
	"strings"
	"testing"

	"github.com/cauebraz/ehlo/internal/event"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte(documented), false)
	if b, err := os.ReadFile("../../../examples/kumomta/records.jsonl"); err == nil {
		for line := range strings.Lines(string(b)) {
			f.Add([]byte(line), true)
		}
		f.Add(b, false)
	}
	for _, s := range []string{
		`[{"type":"Bounce","recipient":["a@gmail.com","b@yahoo.com"],"response":{"code":550,"enhanced_code":{"class":5,"subject":7,"detail":1}},"peer_address":{"name":"mx"}}]`,
		`{"type":"Feedback","feedback_report":{"feedback_type":"abuse","supplemental_trace":{"recipient":7}}}`,
		`{"type":"OOB","response":{"code":-1,"enhanced_code":{"class":9,"subject":-4,"detail":99999}}}`,
		`{"type":"Delivery","timestamp":1e300,"created":-5,"event_time":"x"}`,
		`[]`, `{}`, `[{}, null]`, `nope`,
	} {
		f.Add([]byte(s), false)
	}
	f.Fuzz(func(t *testing.T, body []byte, pool bool) {
		p := Parser{}
		if pool {
			p.By = ByPool
		}
		events, err := p.Parse(body, now)
		if err != nil {
			if events != nil {
				t.Errorf("events %+v returned with error %v", events, err)
			}
			return
		}
		for _, e := range events {
			if e.Time.IsZero() || e.Source == "" || e.Route != "" {
				t.Errorf("event %+v: want a time, a source and no route", e)
			}
			if len(e.Text) > event.MaxTextLength || e.Delay < 0 || e.Count < 0 {
				t.Errorf("event %+v out of bounds", e)
			}
			e.Source = "s"
			if err := e.Validate(); err != nil {
				t.Errorf("event %+v: %v", e, err)
			}
		}
	})
}
