// Command ehlo-sim generates the traffic of a sending platform against an
// ehlo server, for demos and end-to-end tests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cauebraz/ehlo/internal/client"
	"github.com/cauebraz/ehlo/internal/sim"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ehlo-sim:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ehlo-sim", flag.ContinueOnError)
	target := fs.String("target", "http://127.0.0.1:8080", "ehlo server base URL")
	scenario := fs.String("scenario", string(sim.Steady), "scenario: "+strings.Join(sim.ScenarioNames(), ", "))
	sources := fs.String("sources", "mx-1,mx-2,mx-3,mx-4", "comma-separated warm sending identities")
	newSource := fs.String("new-source", "mx-new", "source the warmup scenario adds")
	daily := fs.Float64("daily", 600_000, "messages per simulated day, the baseline share included")
	baselines := fs.String("baselines", "ses,sendgrid", "comma-separated ESP routes for the baseline share, empty for none")
	baselineShare := fs.Float64("baseline-share", 0.10, "share of the volume sent through the baselines")
	queueRunDelay := fs.Duration("queue-run-delay", 5*time.Minute, "deferred queue scan interval (Postfix queue_run_delay)")
	queueLifetime := fs.Duration("queue-lifetime", 24*time.Hour, "deferred mail bounces after this age (maximal_queue_lifetime)")
	dayLength := fs.Duration("day-length", 24*time.Hour, "simulated day for the end-of-day rollup, match the server's -day-length")
	speed := fs.Float64("speed", 60, "simulated seconds per real second")
	tick := fs.Duration("tick", time.Second, "real interval between steps")
	seed := fs.Uint64("seed", 1, "random seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sc, err := sim.ParseScenario(*scenario)
	if err != nil {
		return err
	}
	if *speed <= 0 || *tick <= 0 || *daily <= 0 || *queueRunDelay <= 0 || *queueLifetime <= 0 || *dayLength <= 0 {
		return errors.New("speed, tick, daily, queue-run-delay, queue-lifetime and day-length must be positive")
	}
	cfg := sim.Config{
		Scenario:      sc,
		Sources:       split(*sources),
		DailyVolume:   *daily,
		Baselines:     split(*baselines),
		BaselineShare: *baselineShare,
		QueueRunDelay: *queueRunDelay,
		QueueLifetime: *queueLifetime,
		DayLength:     *dayLength,
		Start:         time.Now().UTC().Truncate(24 * time.Hour).Add(8 * time.Hour),
		Seed:          *seed,
	}
	if sc == sim.Warmup {
		cfg.NewSource = *newSource
	}
	if len(cfg.Sources) == 0 {
		return errors.New("at least one source is required")
	}
	if len(cfg.Baselines) == 0 {
		cfg.BaselineShare = 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := client.New(*target, 5*time.Second)
	s := sim.New(cfg, c)
	step := time.Duration(float64(*tick) * *speed)
	slog.Info("ehlo-sim started", "target", *target, "scenario", sc, "sources", cfg.Sources, "speed", *speed, "sim_start", cfg.Start)
	t := time.NewTicker(*tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			events, errs := s.Step(ctx, step)
			for _, err := range errs {
				slog.Warn("limits", "err", err)
			}
			if err := c.Post(ctx, events); err != nil && ctx.Err() == nil {
				slog.Error("post", "err", err)
			}
		}
	}
}

func split(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
