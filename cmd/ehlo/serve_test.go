package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/client"
	"github.com/cauebraz/ehlo/internal/event"
)

func TestScanLinesSkipsOversizedLine(t *testing.T) {
	in := "first\n" + strings.Repeat("x", maxLine+10) + "\nlast"
	lines, errc := scanLines(context.Background(), strings.NewReader(in))
	var got []string
	for l := range lines {
		got = append(got, l)
	}
	if want := []string{"first", "last"}; !slices.Equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if err := <-errc; err != nil {
		t.Errorf("scan error = %v, want nil", err)
	}
}

type refusingPoster struct{ calls int }

func (r *refusingPoster) Post(context.Context, []event.Event) error {
	r.calls++
	return fmt.Errorf("post: %w", client.ErrRejected)
}

func TestFlushDropsRefusedBatch(t *testing.T) {
	rp := &refusingPoster{}
	b := &batcher{post: rp, batch: 2, max: maxPending}
	b.add(attempts(4))
	if err := b.flush(context.Background()); err != nil {
		t.Fatalf("flush = %v, want nil for a refused batch", err)
	}
	if rp.calls != 2 || len(b.pending) != 0 || b.failed {
		t.Errorf("calls %d pending %d failed %v, want 2 calls, empty buffer, not failed", rp.calls, len(b.pending), b.failed)
	}
}

func TestServeSavesSnapshotOnShutdown(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "warmup.json")
	cfg := filepath.Join(dir, "ehlo.yaml")
	body := "listen: \"127.0.0.1:0\"\nstate_path: " + state + "\ntargets:\n  default: 1000\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, []string{"-config", cfg}) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("snapshot after shutdown: %v", err)
	}
}

func TestServeRejectsBadSendGridKey(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ehlo.yaml")
	if err := os.WriteFile(cfg, []byte("webhooks:\n  sendgrid_public_key: not-base64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serve(context.Background(), []string{"-config", cfg, "-state", filepath.Join(dir, "s.json")})
	if err == nil || !strings.Contains(err.Error(), "sendgrid_public_key") {
		t.Errorf("serve = %v, want a sendgrid_public_key error", err)
	}
}

type sizeLimitPoster struct {
	limit  int
	sizes  []int
	events []event.Event
}

func (s *sizeLimitPoster) Post(_ context.Context, events []event.Event) error {
	s.sizes = append(s.sizes, len(events))
	if len(events) > s.limit {
		return fmt.Errorf("post: %w", client.ErrTooLarge)
	}
	s.events = append(s.events, events...)
	return nil
}

func TestFlushSplitsATooLargeBatch(t *testing.T) {
	sp := &sizeLimitPoster{limit: 3}
	b := &batcher{post: sp, batch: 10, max: maxPending}
	b.add(attempts(10))
	if err := b.flush(context.Background()); err != nil {
		t.Fatalf("flush = %v, want nil", err)
	}
	if len(sp.events) != 10 || len(b.pending) != 0 {
		t.Errorf("delivered %d events, pending %d (posts %v), want all 10 delivered", len(sp.events), len(b.pending), sp.sizes)
	}
	for i, e := range sp.events {
		if e.Count != i+1 {
			t.Fatalf("event %d has count %d, want the original order", i, e.Count)
		}
	}
}

func TestFlushDropsOneEventTooLargeAlone(t *testing.T) {
	sp := &sizeLimitPoster{limit: 0}
	b := &batcher{post: sp, batch: 4, max: maxPending}
	b.add(attempts(2))
	if err := b.flush(context.Background()); err != nil {
		t.Fatalf("flush = %v, want nil", err)
	}
	if len(b.pending) != 0 || len(sp.events) != 0 {
		t.Errorf("pending %d, delivered %d, want both events dropped", len(b.pending), len(sp.events))
	}
}

func TestTailRejectsBatchAboveServerLimit(t *testing.T) {
	err := tailPostfix(context.Background(), []string{"-source", "mx-1", "-batch", "10001", "-file", os.DevNull})
	if err == nil || !strings.Contains(err.Error(), "-batch") {
		t.Errorf("tail-postfix -batch 10001 = %v, want a -batch error", err)
	}
}

func TestTailRejectsNonPositiveFlush(t *testing.T) {
	err := tailPostfix(context.Background(), []string{"-source", "mx-1", "-flush", "0", "-file", os.DevNull})
	if err == nil || !strings.Contains(err.Error(), "-flush") {
		t.Errorf("tail-postfix -flush 0 = %v, want a -flush error", err)
	}
}
