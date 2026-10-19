// Package refresher re-checks registered contracts on a schedule, so the
// registry notices an upgraded contract without anyone asking it to.
//
// A sweep lists every contract on the registry's network whose
// last_refreshed is older than the interval and calls Registry.Refresh on
// each. Refresh settles an unchanged contract with a single RPC round trip,
// so a sweep over a mostly-static registry is cheap.
package refresher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/store"
)

// Registry is the slice of *registry.Registry a sweep needs. It is an
// interface so the scheduling can be tested without a network.
type Registry interface {
	Network(ctx context.Context) (string, error)
	Refresh(ctx context.Context, contractID string) (*registry.Result, error)
	Store() store.Store
}

// Options configures a Refresher.
type Options struct {
	// Interval is both how often a sweep runs and how stale a contract must
	// be before a sweep re-checks it. Must be positive.
	Interval time.Duration
	// Timeout bounds the refresh of a single contract, so one slow RPC
	// response cannot stall the rest of the sweep. Zero means no bound
	// beyond the sweep's own context.
	Timeout time.Duration
	// Logger receives one line per sweep and one per failure. Defaults to
	// slog.Default.
	Logger *slog.Logger
	// Now is the clock; tests replace it. Defaults to time.Now.
	Now func() time.Time
	// Wait pauses between scheduled refreshes. Tests replace it to avoid
	// sleeping; the default respects context cancellation.
	Wait func(context.Context, time.Duration) error
}

// Refresher runs periodic sweeps over the registry.
type Refresher struct {
	reg      Registry
	interval time.Duration
	timeout  time.Duration
	log      *slog.Logger
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
}

// New builds a Refresher. It returns an error for a non-positive interval
// rather than spinning.
func New(reg Registry, opts Options) (*Refresher, error) {
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("refresher: interval must be positive, got %s", opts.Interval)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	wait := opts.Wait
	if wait == nil {
		wait = waitContext
	}
	return &Refresher{
		reg:      reg,
		interval: opts.Interval,
		timeout:  opts.Timeout,
		log:      log,
		now:      now,
		wait:     wait,
	}, nil
}

// Summary reports what one sweep did.
type Summary struct {
	// Checked is how many stale contracts the sweep attempted.
	Checked int `json:"checked"`
	// Changed is how many of them had been upgraded since the last check.
	Changed int `json:"changed"`
	// Failed is how many could not be refreshed. Their errors are logged.
	Failed int `json:"failed"`
	// Busy is how many were not attempted because the registry was at its
	// concurrency limit. They stay stale, so the next sweep picks them up.
	Busy int `json:"busy"`
}

// Run sweeps once immediately and then every interval until ctx is
// cancelled. It returns nil on cancellation; a failed sweep is logged and
// retried on the next tick rather than ending the loop, because a refresher
// that dies on the first RPC outage is worse than none.
func (r *Refresher) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.sweepAndLog(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Refresher) sweepAndLog(ctx context.Context) {
	start := r.now()
	sum, err := r.Sweep(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.log.WarnContext(ctx, "refresh sweep failed", "error", err)
		}
		return
	}
	r.log.InfoContext(ctx, "refresh sweep complete",
		"checked", sum.Checked,
		"changed", sum.Changed,
		"failed", sum.Failed,
		"took", r.now().Sub(start).Round(time.Millisecond),
	)
}

// Sweep refreshes every contract on the registry's network that has not
// been refreshed within the interval.
//
// The stale set is collected in full before any refresh runs. Listings are
// ordered by last_refreshed, and refreshing a contract moves it to the front,
// so paging while refreshing would skip some contracts and revisit others.
//
// A failure on one contract is counted and logged, and the sweep moves on.
// Sweep only returns an error when it cannot work out what to refresh.
func (r *Refresher) Sweep(ctx context.Context) (Summary, error) {
	var sum Summary

	network, err := r.reg.Network(ctx)
	if err != nil {
		return sum, err
	}

	stale, err := r.staleContracts(ctx, network)
	if err != nil {
		return sum, err
	}
	if len(stale) == 0 {
		return sum, nil
	}

	started := r.now()
	count := int64(len(stale))
	step := r.interval / time.Duration(count)
	remainder := int64(r.interval % time.Duration(count))
	var target time.Duration
	var remainderProgress int64
	for i, id := range stale {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if i > 0 {
			target += step
			remainderProgress += remainder
			if remainderProgress >= count {
				target++
				remainderProgress -= count
			}
			if delay := target - r.now().Sub(started); delay > 0 {
				if err := r.wait(ctx, delay); err != nil {
					return sum, err
				}
			}
		}

		sum.Checked++
		result, err := r.refreshOne(ctx, id)
		if err != nil {
			// A busy registry is not a failure: the contract stays stale
			// and the next sweep tries again. Failing it would report a
			// problem where there is none.
			if errors.Is(err, registry.ErrBusy) {
				sum.Busy++
				r.log.DebugContext(ctx, "refresh deferred, registry busy",
					"contract_id", id, "network", network)
				continue
			}
			sum.Failed++
			r.log.WarnContext(ctx, "refresh failed",
				"contract_id", id, "network", network, "error", err)
			continue
		}
		if result.Changed {
			sum.Changed++
		}
	}

	return sum, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Refresher) refreshOne(ctx context.Context, id string) (*registry.Result, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	return r.reg.Refresh(ctx, id)
}

// staleContracts lists the IDs of contracts on network last refreshed
// before now minus the interval.
func (r *Refresher) staleContracts(ctx context.Context, network string) ([]string, error) {
	cutoff := r.now().Add(-r.interval)

	var ids []string
	filter := store.ListFilter{Network: network, Limit: store.MaxLimit}
	for {
		page, err := r.reg.Store().ListContracts(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("refresher: listing contracts: %w", err)
		}
		for _, c := range page.Contracts {
			if c.LastRefreshed.Before(cutoff) {
				ids = append(ids, c.ContractID)
			}
		}

		filter.Offset += len(page.Contracts)
		if len(page.Contracts) == 0 || filter.Offset >= page.Total {
			return ids, nil
		}
	}
}
