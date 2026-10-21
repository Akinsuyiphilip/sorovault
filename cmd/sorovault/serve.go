package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"

	"github.com/soroworks/sorovault/internal/api"
	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/web"
)

// shutdownGrace is how long in-flight requests get to finish after a signal.
const shutdownGrace = 15 * time.Second

// requestTimeout bounds any single request. Registering a contract does two
// RPC round trips, so this is generous compared with a read-only API.
const requestTimeout = 60 * time.Second

func newServeCmd() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the JSON API and browse UI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(cmd.Context(), func(ctx context.Context, a *app) error {
				listenAddr := a.cfg.HTTPAddr
				if addr != "" {
					listenAddr = addr
				}

				refresh, stopRefresh, err := startRefresher(ctx, a)
				if err != nil {
					return err
				}
				// Stopped before withApp closes the store, so a sweep is
				// never left writing to a closed pool.
				defer stopRefresh()

				handler, err := newRouter(a.registry, a.log, refresh)
				if err != nil {
					return err
				}

				return serve(ctx, listenAddr, handler, a.log)
			})
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (overrides HTTP_ADDR)")
	return cmd
}

// startRefresher runs automatic refresh in the background when
// REFRESH_INTERVAL is set. It returns nil when refresh is off, and a stop
// function that cancels it and waits for an in-progress sweep to finish.
func startRefresher(ctx context.Context, a *app) (r *refresher.Refresher, stop func(), err error) {
	if a.cfg.RefreshInterval <= 0 {
		return nil, func() {}, nil
	}

	notifier, err := refreshNotifier(a)
	if err != nil {
		return nil, nil, err
	}

	r, err = refresher.New(a.registry, refresher.Options{
		Interval: a.cfg.RefreshInterval,
		Timeout:  a.cfg.RPCTimeout,
		Notifier: notifier,
		Logger:   a.log.With("component", "refresher"),
	})
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()

	a.log.Info("automatic refresh enabled", "interval", a.cfg.RefreshInterval)
	return r, func() {
		cancel()
		<-done
	}, nil
}

// refreshNotifier builds the webhook a sweep posts upgrades to, if one is
// configured.
func refreshNotifier(a *app) (refresher.Notifier, error) {
	if a.cfg.RefreshWebhookURL == "" {
		return nil, nil
	}

	webhook, err := refresher.NewWebhook(refresher.WebhookOptions{
		URL:    a.cfg.RefreshWebhookURL,
		Secret: a.cfg.RefreshWebhookSecret,
		Logger: a.log.With("component", "refresher-webhook"),
	})
	if err != nil {
		return nil, err
	}

	a.log.Info("refresh webhook enabled", "url", a.cfg.RefreshWebhookURL)
	return webhook, nil
}

// newRouter assembles the JSON API, the browse UI and the health endpoint
// into one handler. refresh may be nil when automatic refresh is off; the
// statistics endpoint then has no sweep to report.
func newRouter(reg *registry.Registry, log *slog.Logger, refresh *refresher.Refresher) (http.Handler, error) {
	ui, err := web.NewServer(reg, log)
	if err != nil {
		return nil, err
	}

	server := api.NewServer(reg, log)
	if refresh != nil {
		server.SetLastSweep(refresh.LastSweep)
	}

	r := chi.NewRouter()
	r.Use(api.Middleware(log)...)

	r.Get("/healthz", healthz)
	r.Mount("/api", server.Routes())
	r.Mount("/", ui.Routes())

	return http.TimeoutHandler(r, requestTimeout, `{"error":"request timed out"}`), nil
}

// healthz reports process liveness. It deliberately does not touch the
// database or RPC: it answers "is this process serving?", which is what a
// scheduler restarts on, not "are its dependencies healthy?".
func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
	})
}

// serve runs the HTTP server until the context is cancelled or a termination
// signal arrives, then drains in-flight requests.
func serve(ctx context.Context, addr string, handler http.Handler, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		// WriteTimeout is left unset: the TimeoutHandler above already
		// bounds each request, and a write deadline here would cut
		// responses off without the clean error message it produces.
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil

	case <-ctx.Done():
		log.Info("shutting down", "grace", shutdownGrace)

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	}
}
