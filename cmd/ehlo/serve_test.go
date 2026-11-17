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
