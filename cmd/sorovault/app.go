package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/soroworks/sorovault/internal/config"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

// app bundles everything a command needs, so each command body can stay
// about what it does rather than how it was wired.
type app struct {
	cfg      *config.Config
	log      *slog.Logger
	registry *registry.Registry

	closers []func() error
}

// newApp loads configuration and connects the registry to Postgres and RPC.
func newApp(ctx context.Context) (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log := cfg.NewLogger(os.Stderr)
	slog.SetDefault(log)

	db, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	fetcher := stellar.NewRPCFetcher(cfg.RPCURL, nil)

	reg := registry.New(fetcher, spec.XDRDecoder{}, db, registry.Options{
		ExpectPassphrase: cfg.NetworkPassphrase,
		Logger:           log,
	})

	return &app{
		cfg:      cfg,
		log:      log,
		registry: reg,
		closers:  []func() error{fetcher.Close, db.Close},
	}, nil
}

// Close releases the app's resources in reverse order of acquisition.
func (a *app) Close() error {
	var firstErr error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// withApp runs fn against a fully wired app, tearing it down afterwards.
func withApp(ctx context.Context, fn func(context.Context, *app) error) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := a.Close(); err != nil {
			a.log.Warn("shutting down", "error", err)
		}
	}()

	return fn(ctx, a)
}

// fetchContext bounds a single contract fetch by the configured timeout.
func (a *app) fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.cfg.RPCTimeout)
}

// requireDatabaseURL produces the same guidance as config.Load for commands
// that need only the database and skip the rest of the wiring.
func requireDatabaseURL() (string, error) {
	url, err := config.DatabaseURLFromEnv()
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("DATABASE_URL or DATABASE_URL_FILE is required")
	}
	return url, nil
}
