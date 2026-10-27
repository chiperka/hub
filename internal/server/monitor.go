package server

import (
	"context"
	"time"

	"chiperka-hub/internal/store"
)

const (
	claimAckTimeout = 60 * time.Second // claimed but never acknowledged → back to queue
	workerTimeout   = 60 * time.Second // no heartbeat → offline, its jobs are retried
	joinedWorkerTTL = 24 * time.Hour   // offline self-registered workers are forgotten after this
)

// RunMonitor performs periodic queue housekeeping until ctx is cancelled.
func (s *Server) RunMonitor(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	s.retention(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.checkQueue(ctx)
		case <-hourly.C:
			s.retention(ctx)
		}
	}
}

func (s *Server) checkQueue(ctx context.Context) {
	now := store.Now()

	if n, err := s.store.ResetStaleClaims(ctx, now-claimAckTimeout.Milliseconds()); err == nil && n > 0 {
		s.log.Warn("requeued unacknowledged jobs", "count", n)
		s.notifier.Notify()
		s.changed("queue")
	}

	if ids, err := s.store.MarkStaleWorkersOffline(ctx, now-workerTimeout.Milliseconds()); err == nil {
		for _, id := range ids {
			s.log.Warn("worker went offline", "worker", id)
			s.failWorkerJobs(id, "worker went offline (no heartbeat)")
		}
		if len(ids) > 0 {
			s.changed("workers")
		}
	}

	// A running job that shows no sign of life well past its timeout is lost.
	stale := s.cfg.JobTimeout + 2*time.Minute
	if jobs, err := s.store.StaleRunningJobs(ctx, now-stale.Milliseconds()); err == nil {
		for i := range jobs {
			s.failJob(&jobs[i], "job timed out (no progress reported by the worker)")
		}
	}
}

func (s *Server) retention(ctx context.Context) {
	s.store.PurgeExpiredSessions(ctx)
	if n, err := s.store.DeleteStaleJoinedWorkers(ctx, store.Now()-joinedWorkerTTL.Milliseconds()); err == nil && n > 0 {
		s.log.Info("removed long-offline joined workers", "count", n)
		s.changed("workers")
	}
	if s.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.RetentionDays).UnixMilli()
	ids, err := s.store.RunIDsOlderThan(ctx, cutoff)
	if err != nil {
		return
	}
	for _, id := range ids {
		if s.store.DeleteRun(ctx, id) == nil {
			s.removeRunFiles(id)
		}
	}
	if len(ids) > 0 {
		s.log.Info("retention: deleted old runs", "count", len(ids), "days", s.cfg.RetentionDays)
		s.changed("runs")
	}
}
