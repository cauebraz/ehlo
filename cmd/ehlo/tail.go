package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cauebraz/ehlo/internal/adapters/postfix"
	"github.com/cauebraz/ehlo/internal/client"
	"github.com/cauebraz/ehlo/internal/event"
)

const (
	// maxPending bounds the events kept while the server is unreachable.
	maxPending   = 20_000
	flushTimeout = 5 * time.Second
	maxLine      = 1 << 20
)

// poster is the part of client.Client the batcher uses.
type poster interface {
	Post(ctx context.Context, events []event.Event) error
}

// batcher buffers events and posts them in batches. After a failed post it
// keeps the buffer, bounded by max, and waits for the next flush instead
// of retrying on every line.
type batcher struct {
	post     poster
	batch    int
	max      int
	pending  []event.Event
	failed   bool
	dropped  int
	reported int
}

// add buffers events. While the server is failing it keeps at most max,
// dropping the oldest, because no post drains the buffer until the next
// flush tick.
func (b *batcher) add(events []event.Event) {
	b.pending = append(b.pending, events...)
	if b.failed {
		b.trim()
	}
}

// trim drops the oldest events once the buffer passes max, down to 90% of
// max, so a full buffer shifts its contents once per max/10 events and not
// on every line.
func (b *batcher) trim() {
	if len(b.pending) <= b.max {
		return
	}
	over := len(b.pending) - (b.max - b.max/10)
	b.dropped += over
	b.pending = slices.Delete(b.pending, 0, over)
}

// reportDrops logs the events dropped since the last report. flush calls
// it once per call.
func (b *batcher) reportDrops() {
	if b.dropped > b.reported {
		slog.Error("dropping events, server unreachable", "dropped", b.dropped-b.reported, "dropped_total", b.dropped)
		b.reported = b.dropped
	}
}

// ready reports whether a line-driven post should happen now.
func (b *batcher) ready() bool {
	return len(b.pending) >= b.batch && !b.failed
}

// flush posts the buffer in batch-sized requests and stops at the first
// error that a retry could fix, trimming the buffer to max. It logs the
// events dropped since its last call at most once. A batch the
// server refused or accepted in part is logged and not sent again.
func (b *batcher) flush(ctx context.Context) error {
	for len(b.pending) > 0 {
		n := min(len(b.pending), b.batch)
		err := b.post.Post(ctx, b.pending[:n])
		for errors.Is(err, client.ErrTooLarge) && n > 1 {
			n /= 2
			err = b.post.Post(ctx, b.pending[:n])
		}
		partial, isPartial := errors.AsType[*client.PartialError](err)
		switch {
		case isPartial:
			slog.Warn("server rejected some events", "accepted", partial.Accepted, "invalid", partial.Invalid, "errors", partial.Errors)
			err = nil
		case errors.Is(err, client.ErrRejected):
			slog.Error("server refused a batch, dropping it", "events", n, "err", err)
			err = nil
		}
		if err != nil {
			b.failed = true
			b.trim()
			b.reportDrops()
			return err
		}
		b.pending = b.pending[n:]
	}
	b.failed = false
	b.reportDrops()
	return nil
}

func tailPostfix(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ehlo tail-postfix", flag.ContinueOnError)
	source := fs.String("source", "", "sending identity for this host (required)")
	target := fs.String("target", "http://127.0.0.1:8080", "ehlo server base URL")
	file := fs.String("file", "-", "log file to read to its end, - for stdin; follow a live log with tail -F | ehlo tail-postfix")
	batch := fs.Int("batch", 200, "events per request, at most 10000; a batch the server finds too large is split")
	flush := fs.Duration("flush", 2*time.Second, "send a partial batch after this long")
	tz := fs.String("tz", "Local", "time zone of syslog timestamps, an IANA name or Local")
	retryAfter := fs.Duration("retry-after", time.Minute, "a delivery whose message waited longer than this before its last active queue entry (the first field of delays=) is a retry")
	skipRelay := fs.String("skip-relay", "", "comma-separated relay hosts (host, host:port or [host]:port) whose replies are not a provider's, such as a smarthost")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		return errors.New("-source is required")
	}
	if *batch < 1 || *batch > event.MaxBatch {
		return fmt.Errorf("-batch must be between 1 and %d", event.MaxBatch)
	}
	in, closeIn, err := openInput(*file)
	if err != nil {
		return err
	}
	defer closeIn()
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("-tz %q: %w", *tz, err)
	}
	parser := &postfix.Parser{Source: *source, Location: loc, RetryAfter: *retryAfter, SkipRelays: splitList(*skipRelay)}
	b := &batcher{post: client.New(*target, 10*time.Second), batch: *batch, max: maxPending}
	return pump(ctx, in, parser, b, *flush)
}

func openInput(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// scanLines reads in on its own goroutine. A line longer than maxLine is
// skipped and counted, so one oversized line never stops the tailer. The
// lines channel closes at EOF or cancellation, and errc then carries the
// read error, nil at EOF.
func scanLines(ctx context.Context, in io.Reader) (<-chan string, <-chan error) {
	lines := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(in, 64*1024)
		skipped := 0
		for {
			line, tooLong, err := readLine(br)
			if tooLong {
				skipped++
				slog.Warn("skipped a log line longer than the limit", "limit_bytes", maxLine, "skipped_total", skipped)
			} else if line != "" || err == nil {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				errc <- err
				return
			}
		}
	}()
	return lines, errc
}

// readLine returns the next line without its newline. When the line is
// longer than maxLine it consumes the rest of it and reports tooLong.
func readLine(br *bufio.Reader) (line string, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLine+1 {
			tooLong = true
			buf = buf[:0]
		} else if !tooLong {
			buf = append(buf, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return "", true, err
		}
		return strings.TrimRight(string(buf), "\r\n"), false, err
	}
}

// pump parses lines into the batcher and posts on size or on the flush
// ticker. On cancellation or EOF it posts what is left with a fresh
// deadline, because ctx is already done.
func pump(ctx context.Context, in io.Reader, parser *postfix.Parser, b *batcher, flush time.Duration) error {
	lines, scanErr := scanLines(ctx, in)
	t := time.NewTicker(flush)
	defer t.Stop()
	final := func() error {
		fctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		defer cancel()
		return b.flush(fctx)
	}
	for {
		select {
		case <-ctx.Done():
			return final()
		case <-t.C:
			if err := b.flush(ctx); err != nil {
				slog.Error("post events", "err", err, "pending", len(b.pending))
			}
		case line, ok := <-lines:
			if !ok {
				err := final()
				select {
				case serr := <-scanErr:
					return errors.Join(err, serr)
				default:
					return err
				}
			}
			b.add(parser.Parse(line))
			if b.ready() {
				if err := b.flush(ctx); err != nil {
					slog.Error("post events", "err", err, "pending", len(b.pending))
				}
			}
		}
	}
}
