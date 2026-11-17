package client_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/client"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/warmup"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func events(n int) []event.Event {
	out := make([]event.Event, n)
	for i := range out {
		out[i] = event.Event{Version: 1, Type: event.Attempt, Time: now, Source: "mx-1", Domain: "gmail.com"}
	}
	return out
}

func TestPostSendsNDJSON(t *testing.T) {
	var gotType, gotPath string
	var lines []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType, gotPath = r.Header.Get("Content-Type"), r.URL.Path
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c := client.New(srv.URL, time.Second)
	if err := c.Post(context.Background(), events(3)); err != nil {
		t.Fatal(err)
	}
	if gotType != "application/x-ndjson" || gotPath != "/v1/events" {
		t.Errorf("request = %s %s, want application/x-ndjson to /v1/events", gotType, gotPath)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	var e event.Event
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.Type != event.Attempt || e.Source != "mx-1" {
		t.Errorf("line 0 = %q (%v), want an attempt from mx-1", lines[0], err)
	}
}

func TestPostErrorCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "line 2: invalid event: source is required", http.StatusBadRequest)
	}))
	defer srv.Close()
	err := client.New(srv.URL, time.Second).Post(context.Background(), events(2))
	if err == nil {
		t.Fatal("expected an error on 400")
	}
	for _, want := range []string{"status 400", "source is required", "post 2 events"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestPostEmptyMakesNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if err := client.New(srv.URL, time.Second).Post(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := client.New(srv.URL, time.Second).Post(context.Background(), []event.Event{}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestDecide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/v1/limits/mx-1/gmail":
			_ = json.NewEncoder(w).Encode(warmup.Decision{Source: "mx-1", Provider: "gmail", Managed: true, State: warmup.Raising, Limit: 70, AllowedNow: 4})
		case "/v1/limits/ghost/gmail":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(warmup.Decision{Source: "ghost", Provider: "gmail"})
		case "/v1/limits/a%2Fb/gmail":
			_ = json.NewEncoder(w).Encode(warmup.Decision{Source: "a/b", Provider: "gmail", Managed: true})
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := client.New(srv.URL, time.Second)
	ctx := context.Background()

	d, err := c.Decide(ctx, "mx-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Managed || d.State != warmup.Raising || d.Limit != 70 || d.AllowedNow != 4 {
		t.Errorf("decision = %+v", d)
	}

	d, err = c.Decide(ctx, "ghost", "gmail")
	if err != nil {
		t.Fatalf("404 must not be an error: %v", err)
	}
	if d.Managed || d.Source != "ghost" {
		t.Errorf("unmanaged decision = %+v", d)
	}

	d, err = c.Decide(ctx, "a/b", "gmail")
	if err != nil || !d.Managed {
		t.Errorf("escaped source: %+v, %v", d, err)
	}

	_, err = c.Decide(ctx, "mx-2", "gmail")
	if err == nil || !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("500 error = %v, want status 500 with body", err)
	}
}

func TestDecideRejectsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{not json")
	}))
	defer srv.Close()
	if _, err := client.New(srv.URL, time.Second).Decide(context.Background(), "mx-1", "gmail"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestProbe(t *testing.T) {
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := client.Probe(ctx, srv.URL+"/healthz", time.Second); err != nil {
		t.Errorf("200: %v", err)
	}
	status.Store(http.StatusServiceUnavailable)
	if err := client.Probe(ctx, srv.URL+"/healthz", time.Second); err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Errorf("503 error = %v", err)
	}
}

func TestProbeHonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.Probe(ctx, srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error = %v, want context canceled", err)
	}
}

func TestPostReportsPartialWithLongErrors(t *testing.T) {
	long := strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		errs := make([]string, 10)
		for i := range errs {
			errs[i] = long
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": 1, "invalid": 10, "errors": errs})
	}))
	defer srv.Close()
	err := client.New(srv.URL, time.Second).Post(t.Context(), events(11))
	partial, ok := errors.AsType[*client.PartialError](err)
	if !ok {
		t.Fatalf("Post error = %v, want *PartialError", err)
	}
	if partial.Accepted != 1 || partial.Invalid != 10 {
		t.Errorf("partial = %d accepted, %d invalid, want 1 and 10", partial.Accepted, partial.Invalid)
	}
}

func TestPostStatusDecidesWhetherToRetry(t *testing.T) {
	for _, tc := range []struct {
		code               int
		rejected, tooLarge bool
	}{
		{http.StatusBadRequest, true, false},
		{http.StatusRequestEntityTooLarge, true, true},
		{http.StatusUnsupportedMediaType, true, false},
		{http.StatusUnprocessableEntity, true, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusForbidden, false, false},
		{http.StatusNotFound, false, false},
		{http.StatusRequestTimeout, false, false},
		{http.StatusTooManyRequests, false, false},
		{http.StatusBadGateway, false, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		err := client.New(srv.URL, time.Second).Post(context.Background(), events(1))
		srv.Close()
		if err == nil {
			t.Errorf("%d: no error", tc.code)
			continue
		}
		if got := errors.Is(err, client.ErrRejected); got != tc.rejected {
			t.Errorf("%d: rejected = %v, want %v (%v)", tc.code, got, tc.rejected, err)
		}
		if got := errors.Is(err, client.ErrTooLarge); got != tc.tooLarge {
			t.Errorf("%d: too large = %v, want %v (%v)", tc.code, got, tc.tooLarge, err)
		}
	}
}
