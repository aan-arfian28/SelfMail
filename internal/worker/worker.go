// Package worker is the single in-process job consumer (FR-17).
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"selfmail/internal/config"
	"selfmail/internal/service"
	"selfmail/internal/store"
)

type Worker struct {
	Svc *service.Service
	St  *store.Store
	Cfg *config.Config
	Log *slog.Logger
	id  string
}

func New(svc *service.Service, st *store.Store, cfg *config.Config, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	return &Worker{Svc: svc, St: st, Cfg: cfg, Log: log, id: fmt.Sprintf("%s-%d", host, os.Getpid())}
}

// Backoff is exponential: base * 2^(attempt-1), capped at 1h.
func Backoff(base time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if d > time.Hour || d <= 0 {
		d = time.Hour
	}
	return d
}

func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(w.Cfg.WorkerPollInterval)
	defer tick.Stop()
	recon := time.NewTicker(5 * time.Minute)
	defer recon.Stop()
	w.Log.Info("worker started", "id", w.id, "poll", w.Cfg.WorkerPollInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-recon.C:
			if open, err := w.St.HasOpenJob(ctx, "reconcile", nil); err == nil && !open {
				_, _ = w.St.Enqueue(ctx, "reconcile", nil, nil, nil, time.Now())
			}
		case <-tick.C:
			if n, err := w.St.RecoverStuckJobs(ctx, w.Cfg.JobLockTimeout); err != nil {
				w.Log.Error("recover stuck jobs", "err", err)
			} else if n > 0 {
				w.Log.Warn("recovered stuck jobs", "count", n)
			}
			for { // drain all due jobs
				if !w.runOne(ctx) {
					break
				}
			}
		}
	}
}

func (w *Worker) runOne(ctx context.Context) bool {
	j, err := w.St.ClaimJob(ctx, w.id)
	if err != nil {
		w.Log.Error("claim job", "err", err)
		return false
	}
	if j == nil {
		return false
	}
	jctx, cancel := context.WithTimeout(ctx, w.Cfg.JobLockTimeout/2)
	defer cancel()
	if err := w.Svc.RunJob(jctx, j); err != nil {
		next := time.Now().Add(Backoff(w.Cfg.JobBaseBackoff, j.Attempts))
		w.Log.Warn("job failed", "id", j.ID, "type", j.Type, "attempt", j.Attempts, "err", err)
		if ferr := w.St.FailJob(ctx, j, err, next); ferr != nil {
			w.Log.Error("record job failure", "err", ferr)
		}
		return true
	}
	if err := w.St.CompleteJob(ctx, j.ID); err != nil {
		w.Log.Error("complete job", "err", err)
	}
	return true
}
