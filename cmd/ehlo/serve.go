package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/cauebraz/ehlo/internal/config"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/server"
	"github.com/cauebraz/ehlo/internal/warmup"
)

const (
	shutdownTimeout = 10 * time.Second
	// maxHeaderBytes bounds the request line and headers. net/http allows
	// http.Server.MaxHeaderBytes plus headerSlack bytes, so the server
	// sets the difference.
	maxHeaderBytes = 64 << 10
	headerSlack    = 4096
)

func serve(ctx context.Context, args []string) error {
	cfg, err := loadServeConfig(args)
	if err != nil {
		return err
	}
	cls := provider.New(cfg.ProviderRules())
	eng, err := warmup.New(cfg.Warmup, cfg.Targets, cls.Names(), warmup.WithKnownSources(cfg.Sources))
	if err != nil {
		return err
	}
	store := warmup.FileStore{Path: cfg.StatePath}
	lanes, err := restore(eng, store)
	if err != nil {
		return err
	}
	reg, m := buildRegistry(cfg, cls, eng)
	saveSnapshot(eng, store, m)
	opts, err := serverOptions(cfg)
	if err != nil {
		return err
	}
	pipeline := ingest.New(cls, m, eng, ingest.WithRoutes(cfg.Routes), ingest.WithSources(cfg.MaxSources, seenSources(cfg, eng)))
	srv := server.New(pipeline, eng, reg, opts...)
	httpSrv := newHTTPServer(cfg, srv.Handler())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { snapshotLoop(ctx, eng, store, cfg.Snapshot, m) })
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	slog.Info("ehlo serving", "listen", cfg.Listen, "version", buildVersion(), "lanes", lanes, "state", cfg.StatePath)

	select {
	case err := <-errc:
		cancel()
		wg.Wait()
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	case <-ctx.Done():
	}
	return shutdown(httpSrv, errc, &wg, eng, store)
}

// newHTTPServer bounds every connection: header size, read, write and idle
// time.
func newHTTPServer(cfg config.Config, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes - headerSlack,
	}
}

func loadServeConfig(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("ehlo serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML config file, defaults apply when empty")
	listen := fs.String("listen", "", "override listen address")
	statePath := fs.String("state", "", "override state_path")
	dayLength := fs.Duration("day-length", 0, "override warmup.day_length, for demos")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return cfg, err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *statePath != "" {
		cfg.StatePath = *statePath
	}
	if *dayLength != 0 {
		cfg.Warmup.DayLength = *dayLength
	}
	return cfg, cfg.Validate()
}

// serverOptions turns the config into server options, parsing the
// SendGrid key once at start so a bad key fails the start.
func serverOptions(cfg config.Config) ([]server.Option, error) {
	opts := []server.Option{server.WithMaxBody(cfg.MaxBodyBytes)}
	if cfg.Webhooks.Token != "" {
		opts = append(opts, server.WithWebhookToken(cfg.Webhooks.Token))
	}
	if cfg.Webhooks.SendGridPublicKey != "" {
		k, err := server.ParseSendGridKey(cfg.Webhooks.SendGridPublicKey)
		if err != nil {
			return nil, fmt.Errorf("webhooks.sendgrid_public_key: %w", err)
		}
		opts = append(opts, server.WithSendGridKey(k))
	}
	return opts, nil
}

// seenSources lists the sources the config names and the restored lanes
// hold, so a restart does not count them again against max_sources.
func seenSources(cfg config.Config, eng *warmup.Engine) []string {
	seen := slices.Clone(cfg.Sources)
	seen = append(seen, cfg.Targets.Warm...)
	for name := range cfg.Targets.Sources {
		seen = append(seen, name)
	}
	for _, l := range eng.Snapshot().Lanes {
		seen = append(seen, l.Source)
	}
	return seen
}

// restore loads the saved lanes and returns how many came back.
func restore(eng *warmup.Engine, store warmup.Store) (int, error) {
	snap, err := store.Load()
	if err != nil {
		return 0, err
	}
	dropped, err := eng.Restore(snap)
	if err != nil {
		return 0, fmt.Errorf("restore warmup state: %w", err)
	}
	if dropped > 0 {
		slog.Warn("dropped lanes whose source or provider is no longer configured", "dropped", dropped)
	}
	return len(snap.Lanes) - dropped, nil
}

func buildRegistry(cfg config.Config, cls *provider.Classifier, eng *warmup.Engine) (*prometheus.Registry, *metrics.Metrics) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg, buildVersion())
	for _, src := range cfg.Sources {
		m.Prime(event.RouteSelfHosted, src, cls.Names())
	}
	reg.MustRegister(metrics.NewWarmup(eng.Lanes))
	return reg, m
}

// snapshotLoop rolls the policy day and saves the lanes every interval,
// so a restart loses at most one interval of counters.
func snapshotLoop(ctx context.Context, eng *warmup.Engine, store warmup.Store, every time.Duration, m *metrics.Metrics) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			eng.Roll()
			saveSnapshot(eng, store, m)
		}
	}
}

// saveSnapshot saves the lanes and exports the result, so a state file
// that cannot be written raises EhloWarmupStateNotSaved.
func saveSnapshot(eng *warmup.Engine, store warmup.Store, m *metrics.Metrics) {
	err := store.Save(eng.Snapshot())
	m.Snapshot(err)
	if err != nil {
		slog.Error("save warmup snapshot", "err", err)
	}
}

// shutdown stops accepting requests, waits for the snapshot loop, saves the
// lanes one last time and reports every error at once.
func shutdown(httpSrv *http.Server, errc <-chan error, wg *sync.WaitGroup, eng *warmup.Engine, store warmup.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := httpSrv.Shutdown(ctx)
	wg.Wait()
	if saveErr := store.Save(eng.Snapshot()); saveErr != nil {
		err = errors.Join(err, saveErr)
	}
	if srvErr := <-errc; !errors.Is(srvErr, http.ErrServerClosed) {
		err = errors.Join(err, srvErr)
	}
	return err
}
