// Command ehlo serves the ingest API, the limits API and the metrics
// endpoint, and tails Postfix logs into events.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/cauebraz/ehlo/internal/client"
)

var version = "dev"

// buildVersion is the -X main.version value, or the module version Go
// recorded when the binary was installed with go install.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

const usage = `usage: ehlo <command> [flags]

commands:
  serve         run the server: POST /v1/events, POST /v1/webhooks/{ses,sendgrid,kumomta}, GET /v1/limits, GET /metrics, GET /healthz
  tail-postfix  read Postfix log lines from stdin or a file and post them as events
  probe         GET a URL and exit 0 on HTTP 200 (container health check)
  version       print the version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches one command and returns the exit code, so every deferred
// call has run before the process exits.
func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, args[1:])
	case "tail-postfix":
		err = tailPostfix(ctx, args[1:])
	case "probe":
		err = probe(ctx, args[1:])
	case "version":
		fmt.Println(buildVersion())
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ehlo:", err)
		return 1
	}
	return 0
}

func probe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ehlo probe", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 3*time.Second, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ehlo probe <url>")
	}
	return client.Probe(ctx, fs.Arg(0), *timeout)
}
