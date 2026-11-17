package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cauebraz/ehlo/internal/adapters/postfix"
	"github.com/cauebraz/ehlo/internal/config"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/warmup"
)

func TestAddBoundsPendingWhileFailed(t *testing.T) {
	fp := &fakePoster{failOn: []int{0}}
	b := &batcher{post: fp, batch: 2, max: 10}
	b.add(attempts(3))
	if err := b.flush(t.Context()); err == nil {
		t.Fatal("flush succeeded, want the first post to fail")
	}
	for range 100 {
		b.add(attempts(5))
	}
	if len(b.pending) > b.max {
		t.Errorf("pending = %d while failed, want at most max %d", len(b.pending), b.max)
	}
	if want := 3 + 500 - len(b.pending); b.dropped != want {
		t.Errorf("dropped = %d, want %d", b.dropped, want)
	}
	if last := b.pending[len(b.pending)-1]; last.Count != 5 {
		t.Errorf("newest kept event Count = %d, want 5: the oldest events are dropped first", last.Count)
	}
}

func TestHTTPServerBoundsHeaders(t *testing.T) {
	cfg := config.Default()
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = newHTTPServer(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.Start()
	defer srv.Close()
	for _, tc := range []struct {
		name string
		size int
		want int
	}{
		{"small header", 1 << 10, http.StatusNoContent},
		{"header over the limit", 2 * maxHeaderBytes, http.StatusRequestHeaderFieldsTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Padding", strings.Repeat("a", tc.size))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if s := srv.Config; s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 {
		t.Errorf("timeouts = %v/%v/%v/%v, want all set", s.ReadHeaderTimeout, s.ReadTimeout, s.WriteTimeout, s.IdleTimeout)
	}
}

func TestTrimDropsAChunkAndLogsOncePerFlush(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	fp := &fakePoster{failOn: []int{0, 1}}
	const max = 1000
	b := &batcher{post: fp, batch: 2, max: max}
	b.add(attempts(1))
	if err := b.flush(t.Context()); err == nil {
		t.Fatal("flush succeeded, want the first post to fail")
	}
	trims := 0
	for range 10 * max {
		before := b.dropped
		b.add(attempts(1))
		if b.dropped != before {
			trims++
		}
		if len(b.pending) > max {
			t.Fatalf("pending = %d, want at most %d", len(b.pending), max)
		}
	}
	if limit := 10*max/(max/10) + 1; trims > limit {
		t.Errorf("trimmed %d times for %d events, want at most %d", trims, 10*max, limit)
	}
	if n := strings.Count(logs.String(), "dropping events"); n != 0 {
		t.Errorf("logged %d drop lines between flushes, want 0", n)
	}
	if err := b.flush(t.Context()); err == nil {
		t.Fatal("flush succeeded, want the second post to fail")
	}
	if n := strings.Count(logs.String(), "dropping events"); n != 1 {
		t.Errorf("logged %d drop lines in one flush, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), fmt.Sprintf("dropped_total=%d", b.dropped)) {
		t.Errorf("drop line lacks dropped_total=%d:\n%s", b.dropped, logs.String())
	}
}

func TestHTTPServerHeaderLimitIsExact(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = newHTTPServer(config.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.Start()
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	head := func(size int) string {
		prefix := "GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\nX-Padding: "
		suffix := "\r\n\r\n"
		return prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix
	}
	for _, tc := range []struct {
		size int
		want int
	}{
		{maxHeaderBytes, http.StatusNoContent},
		{maxHeaderBytes + 1, http.StatusRequestHeaderFieldsTooLarge},
	} {
		conn, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, head(tc.size)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("request head of %d bytes = %d, want %d", tc.size, resp.StatusCode, tc.want)
		}
	}
}

type countingStore struct {
	mu    sync.Mutex
	saves int
}

func (*countingStore) Load() (warmup.Snapshot, error) { return warmup.Snapshot{}, nil }

func (c *countingStore) Save(warmup.Snapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	return nil
}

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saves
}

func TestSnapshotLoopSavesEveryIntervalUntilCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{Default: 1000}, []string{"gmail", "other"})
		if err != nil {
			t.Fatal(err)
		}
		store := &countingStore{}
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() { snapshotLoop(ctx, eng, store, 30*time.Second) })
		time.Sleep(95 * time.Second)
		synctest.Wait()
		if got := store.count(); got != 3 {
			t.Errorf("saves after 95s at 30s = %d, want 3", got)
		}
		cancel()
		wg.Wait()
		time.Sleep(time.Minute)
		if got := store.count(); got != 3 {
			t.Errorf("saves after cancel = %d, want still 3", got)
		}
	})
}

func TestPumpFlushesPartialBatchOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		line := `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`
		pr, pw := io.Pipe()
		fp := &lockedPoster{}
		b := &batcher{post: fp, batch: 100, max: maxPending}
		parser := &postfix.Parser{Source: "mx-1", Year: 2026, Location: time.UTC}
		done := make(chan error, 1)
		go func() { done <- pump(t.Context(), pr, parser, b, 2*time.Second) }()
		if _, err := io.WriteString(pw, line+"\n"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := fp.sizes(); len(got) != 0 {
			t.Fatalf("posted %v before the tick, want nothing below the batch size", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got, want := fp.sizes(), []int{2}; !slices.Equal(got, want) {
			t.Errorf("posted %v after one tick, want %v", got, want)
		}
		_ = pw.Close()
		if err := <-done; err != nil {
			t.Errorf("pump = %v at EOF", err)
		}
	})
}

type lockedPoster struct {
	mu sync.Mutex
	fp fakePoster
}

func (l *lockedPoster) Post(ctx context.Context, events []event.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fp.Post(ctx, events)
}

func (l *lockedPoster) sizes() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.fp.sizes)
}
