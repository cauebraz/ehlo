// Package client talks to an ehlo server: it posts events and asks for
// limits. The tailer and the simulator use it.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// maxErrorBody bounds how much of an error response is read into the error,
// and maxResultBody how much of a 2xx ingest result is decoded.
const (
	maxErrorBody  = 4096
	maxResultBody = 64 << 10
)

// ErrRejected wraps a 4xx answer other than 408 and 429: the server read
// the batch and refused it, so sending it again gives the same answer.
var ErrRejected = errors.New("rejected by the server")

// PartialError reports a batch the server accepted only in part. The
// accepted events are recorded; retrying would count them twice.
type PartialError struct {
	Accepted int      `json:"accepted"`
	Invalid  int      `json:"invalid"`
	Errors   []string `json:"errors"`
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("server accepted %d events and rejected %d: %v", e.Accepted, e.Invalid, e.Errors)
}

// Client is bound to one server base URL.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for base, such as http://127.0.0.1:8080.
func New(base string, timeout time.Duration) *Client {
	return &Client{base: base, http: &http.Client{Timeout: timeout}}
}

// Decide asks the limits API for one lane. An unmanaged pair returns a
// decision with Managed false and no error.
func (c *Client) Decide(ctx context.Context, source, provider string) (warmup.Decision, error) {
	var d warmup.Decision
	u := fmt.Sprintf("%s/v1/limits/%s/%s", c.base, url.PathEscape(source), url.PathEscape(provider))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return d, fmt.Errorf("limits %s/%s: build request: %w", source, provider, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return d, fmt.Errorf("limits %s/%s: %w", source, provider, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return d, fmt.Errorf("limits %s/%s: %w", source, provider, statusError(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return d, fmt.Errorf("limits %s/%s: decode: %w", source, provider, err)
	}
	return d, nil
}

// Post sends events as NDJSON. An empty slice is a no-op. A refused batch
// returns an error wrapping ErrRejected, and a batch with some invalid
// events returns a *PartialError.
func (c *Client) Post(ctx context.Context, events []event.Event) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/events", &buf)
	if err != nil {
		return fmt.Errorf("post %d events: build request: %w", len(events), err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post %d events: %w", len(events), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		err := statusError(resp)
		if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %w", ErrRejected, err)
		}
		return fmt.Errorf("post %d events: %w", len(events), err)
	}
	var res PartialError
	if json.NewDecoder(io.LimitReader(resp.Body, maxResultBody)).Decode(&res) == nil && res.Invalid > 0 {
		return &res
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Probe GETs a URL and returns nil on HTTP 200. Container health checks
// use it.
func Probe(ctx context.Context, u string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("probe %s: %w", u, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: %w", u, statusError(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if msg := bytes.TrimSpace(body); len(msg) > 0 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}
