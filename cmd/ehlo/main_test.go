package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/adapters/postfix"
	"github.com/cauebraz/ehlo/internal/event"
)

// fakePoster records the size of every batch and fails the calls whose
// index is in failOn.
type fakePoster struct {
	sizes  []int
	failOn []int
	events []event.Event
}

var errPost = errors.New("server unreachable")

func (f *fakePoster) Post(_ context.Context, events []event.Event) error {
	call := len(f.sizes)
	f.sizes = append(f.sizes, len(events))
	if slices.Contains(f.failOn, call) {
		return errPost
	}
	f.events = append(f.events, events...)
	return nil
}

func attempts(n int) []event.Event {
	out := make([]event.Event, n)
	for i := range out {
		out[i] = event.Event{Type: event.Attempt, Source: "mx-1", Count: i + 1}
	}
	return out
}

func TestFlushPostsInBatches(t *testing.T) {
	fp := &fakePoster{}
	b := &batcher{post: fp, batch: 2, max: 100}
	b.add(attempts(5))
	if err := b.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []int{2, 2, 1}; !slices.Equal(fp.sizes, want) {
		t.Errorf("batch sizes = %v, want %v", fp.sizes, want)
	}
	if len(b.pending) != 0 || b.failed || b.dropped != 0 {
		t.Errorf("after flush: pending=%d failed=%v dropped=%d", len(b.pending), b.failed, b.dropped)
	}
	if len(fp.events) != 5 || fp.events[4].Count != 5 {
		t.Errorf("posted %d events in order? last=%+v", len(fp.events), fp.events[len(fp.events)-1])
	}
}

func TestFlushStopsAtFirstErrorAndTrims(t *testing.T) {
	fp := &fakePoster{failOn: []int{1}}
	b := &batcher{post: fp, batch: 2, max: 2}
	b.add(attempts(5))
	if !b.ready() {
		t.Fatal("ready() = false with 5 pending and batch 2")
	}
	err := b.flush(context.Background())
	if !errors.Is(err, errPost) {
		t.Fatalf("error = %v, want errPost", err)
	}
	if want := []int{2, 2}; !slices.Equal(fp.sizes, want) {
		t.Errorf("batch sizes = %v, want %v (stopped at the failed call)", fp.sizes, want)
	}
	if !b.failed {
		t.Error("failed not set after an error")
	}
	if b.dropped != 1 || len(b.pending) != 2 {
		t.Errorf("dropped=%d pending=%d, want 1 dropped and 2 kept", b.dropped, len(b.pending))
	}
	if b.pending[0].Count != 4 || b.pending[1].Count != 5 {
		t.Errorf("kept the oldest events: %+v", b.pending)
	}
	if b.ready() {
		t.Error("ready() = true while failed")
	}

	fp.failOn = nil
	if err := b.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.failed || len(b.pending) != 0 {
		t.Errorf("after a successful flush: failed=%v pending=%d", b.failed, len(b.pending))
	}
	b.add(attempts(2))
	if !b.ready() {
		t.Error("ready() = false after the success reset failed")
	}
}

func TestReadyBelowBatch(t *testing.T) {
	b := &batcher{post: &fakePoster{}, batch: 3, max: 10}
	b.add(attempts(2))
	if b.ready() {
		t.Error("ready() = true below the batch size")
	}
}

func TestPumpPostsParsedEventsAtEOF(t *testing.T) {
	lines := strings.Join([]string{
		`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK  1710058502 x12si345678wrs.123 - gsmtp)`,
		`Mar 10 08:15:03 mx-1 postfix/smtp[2231]: 4F3A1B2C3E: to=<user2@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=4.7.28, status=deferred (host gmail-smtp-in.l.google.com[203.0.113.26] said: 421-4.7.28 Our system has detected an unusual rate of unsolicited mail (in reply to RCPT TO command))`,
		`Mar 10 08:15:04 mx-1 postfix/qmgr[2200]: 4F3A1B2C3F: removed`,
	}, "\n") + "\n"
	fixed := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	parser := &postfix.Parser{Source: "mx-1", Now: func() time.Time { return fixed }, Year: 2026, Location: time.UTC}
	fp := &fakePoster{}
	b := &batcher{post: fp, batch: 200, max: maxPending}
	if err := pump(context.Background(), strings.NewReader(lines), parser, b, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(fp.events) != 4 {
		t.Fatalf("posted %d events, want attempt and reply for each delivery line", len(fp.events))
	}
	if want := []int{4}; !slices.Equal(fp.sizes, want) {
		t.Errorf("batch sizes = %v, want one final flush of %v", fp.sizes, want)
	}
	types := make([]event.Type, 0, 4)
	for _, e := range fp.events {
		types = append(types, e.Type)
		if e.Source != "mx-1" || e.Domain != "gmail.com" {
			t.Errorf("event = %+v, want source mx-1 to gmail.com", e)
		}
	}
	if want := []event.Type{event.Attempt, event.Reply, event.Attempt, event.Reply}; !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v", types, want)
	}
	if r := fp.events[3]; r.Code != 421 || r.Enhanced != "4.7.28" {
		t.Errorf("second reply = %+v, want 421 4.7.28", r)
	}
}

func TestPumpFlushesOnBatchSize(t *testing.T) {
	line := `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`
	parser := &postfix.Parser{Source: "mx-1", Year: 2026, Location: time.UTC}
	fp := &fakePoster{}
	b := &batcher{post: fp, batch: 2, max: maxPending}
	if err := pump(context.Background(), strings.NewReader(line+"\n"), parser, b, time.Hour); err != nil {
		t.Fatal(err)
	}
	if want := []int{2}; !slices.Equal(fp.sizes, want) {
		t.Errorf("batch sizes = %v, want %v posted when the batch filled", fp.sizes, want)
	}
}

func TestPumpStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fp := &fakePoster{}
	b := &batcher{post: fp, batch: 10, max: maxPending}
	done := make(chan error, 1)
	go func() { done <- pump(ctx, blockingReader{}, &postfix.Parser{Source: "mx-1"}, b, time.Hour) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("pump returned %v on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not return after cancellation")
	}
}

// blockingReader never delivers data, like an idle pipe.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	select {}
}

func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, 2},
		{"version", []string{"version"}, 0},
		{"bogus", []string{"bogus"}, 2},
		{"probe without url", []string{"probe"}, 1},
		{"tail without source", []string{"tail-postfix"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestBuildVersion(t *testing.T) {
	if v := buildVersion(); v == "" {
		t.Error("buildVersion() is empty")
	}
	old := version
	defer func() { version = old }()
	version = "v9.9.9"
	if v := buildVersion(); v != "v9.9.9" {
		t.Errorf("buildVersion() = %q with -X main.version set, want v9.9.9", v)
	}
}

func TestProbeCommand(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer bad.Close()
	ctx := context.Background()
	if err := probe(ctx, []string{ok.URL}); err != nil {
		t.Errorf("probe 200: %v", err)
	}
	if err := probe(ctx, []string{"-timeout", "1s", ok.URL}); err != nil {
		t.Errorf("probe with flag: %v", err)
	}
	if err := probe(ctx, []string{bad.URL}); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("probe 500 = %v, want status 500", err)
	}
	if err := probe(ctx, []string{}); err == nil {
		t.Error("probe without a url must fail")
	}
	if err := probe(ctx, []string{ok.URL, bad.URL}); err == nil {
		t.Error("probe with two urls must fail")
	}
	if run([]string{"probe", ok.URL}) != 0 {
		t.Error("run probe against 200 != 0")
	}
	if run([]string{"probe", bad.URL}) != 1 {
		t.Error("run probe against 500 != 1")
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"a", []string{"a"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{",relay.example,", []string{"relay.example"}},
	}
	for _, tc := range cases {
		if got := splitList(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestOpenInput(t *testing.T) {
	in, closeIn, err := openInput("-")
	if err != nil {
		t.Fatal(err)
	}
	closeIn()
	if in != os.Stdin {
		t.Error("openInput(\"-\") is not stdin")
	}

	p := filepath.Join(t.TempDir(), "mail.log")
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, closeF, err := openInput(p)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Error("openInput(file) returned nil reader")
	}
	closeF()

	if _, _, err := openInput(filepath.Join(t.TempDir(), "missing.log")); err == nil || !strings.Contains(err.Error(), "missing.log") {
		t.Errorf("missing file error = %v, want it to name the path", err)
	}
}

func TestScanLines(t *testing.T) {
	lines, errc := scanLines(context.Background(), strings.NewReader("a\nb\n"))
	var got []string
	for l := range lines {
		got = append(got, l)
	}
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if err := <-errc; err != nil {
		t.Errorf("scan error = %v", err)
	}
}
