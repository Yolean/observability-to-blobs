package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"
)

// The loop's cadence (compactor/README.md, "Compaction", decision 3):
//
//   - every ROLLING_INTERVAL, the rolling pass folds the current hour, and
//     the previous hour until its final fold has committed;
//   - every PAST_INTERVAL, and on the first tick after a start, the past
//     pass lists raw1/{cluster}/ once and folds every other hour with keys
//     its sidecar lacks, or that is past H+1h+GRACE and not final yet;
//   - a rollup of a past day runs after the rolling pass committed the
//     final fold of that day's last hour, and after every past pass for
//     every past day with hourly sidecars, where it does nothing unless an
//     hour has keys the daily sidecar lacks.
//
// Nothing is remembered between ticks that the bucket does not say, so a
// start is an ordinary tick. One writer: replicas 1, strategy Recreate.
func (c *compactor) run(ctx context.Context, cfg config) error {
	srv := &http.Server{Addr: cfg.metricsAddr, Handler: c.m, ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()
	defer srv.Close()
	c.log.Info("run", "cluster", c.l.cluster, "rolling", cfg.rolling.String(), "grace", cfg.grace.String(),
		"past", cfg.past.String(), "raw_delete", cfg.rawDelete, "metrics", cfg.metricsAddr)
	tick := time.NewTicker(cfg.rolling)
	defer tick.Stop()
	// A stop (SIGTERM) lets the pass in flight finish rather than leave
	// objects ahead of their sidecar; that is harmless, the next fold
	// repeats the keys, but a stopped compactor then leaves a quiet
	// bucket. Each job keeps its own deadline.
	jobs := context.WithoutCancel(ctx)
	var lastPast time.Time
	for {
		now := time.Now().UTC()
		c.rollingPass(jobs, now, cfg.jobTimeout)
		if ctx.Err() == nil && (lastPast.IsZero() || now.Sub(lastPast) >= cfg.past) {
			c.pastPass(jobs, now, cfg.jobTimeout)
			lastPast = now
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-srvErr:
			return err
		case <-tick.C:
		}
	}
}

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// foldJob is fold with a deadline, logged and counted.
func (c *compactor) foldJob(ctx context.Context, h time.Time, listed []Object, now time.Time, timeout time.Duration) (foldResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	r, err := c.fold(ctx, h, listed, now)
	hour := h.Format(dateLayout + "T15")
	if err != nil {
		c.log.Error("fold", "hour", hour, "err", err)
		return r, err
	}
	c.m.fold(r)
	if r.wrote || len(r.deleted) > 0 {
		c.log.Info("fold", "hour", hour, "keys", r.keys, "full", r.full, "final", r.final, "deleted", len(r.deleted),
			"logs", r.rows["logs"], "metrics", r.rows["metrics"], "podindex", r.rows["podindex"], "pods", r.rows["pods"],
			"seconds", time.Since(start).Seconds())
	}
	return r, nil
}

// rollupJob is rollup with a deadline, logged and counted.
func (c *compactor) rollupJob(ctx context.Context, day time.Time, timeout time.Duration) (rollupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	r, err := c.rollup(ctx, day)
	c.m.job("rollup", start, err)
	if err != nil {
		c.log.Error("rollup", "day", day.Format(dateLayout), "err", err)
		return r, err
	}
	c.m.rollup(r)
	if len(r.hours) > 0 {
		c.log.Info("rollup", "day", day.Format(dateLayout), "hours", len(r.hours), "namespaces", r.namespaces,
			"logs", r.rows["logs"], "metrics", r.rows["metrics"], "podindex", r.rows["podindex"], "pods", r.rows["pods"],
			"seconds", time.Since(start).Seconds())
	}
	return r, nil
}

// rollingPass folds the current hour and, until it is final, the previous.
func (c *compactor) rollingPass(ctx context.Context, now time.Time, timeout time.Duration) error {
	start := time.Now()
	cur := now.UTC().Truncate(time.Hour)
	prev := cur.Add(-time.Hour)
	_, err := c.foldJob(ctx, cur, nil, now, timeout)
	errs := []error{err}
	if prev.After(c.finalSeen) {
		r, err := c.foldJob(ctx, prev, nil, now, timeout)
		errs = append(errs, err)
		if err == nil && r.final {
			c.finalSeen = prev
			// the day's last hour just became final: the day's first rollup
			if r.wrote && r.full && dayOf(prev).Before(dayOf(cur)) {
				_, err := c.rollupJob(ctx, dayOf(prev), timeout)
				errs = append(errs, err)
			}
		}
	}
	err = errors.Join(errs...)
	c.m.job("rolling", start, err)
	return err
}

// pastPass deletes abandoned temporary objects, lists the cluster's raw
// prefix once, folds every hour but the current one that has new keys or is
// due its final fold, sets the raw gauges from the listing, and rolls up
// every past day.
func (c *compactor) pastPass(ctx context.Context, now time.Time, timeout time.Duration) error {
	start := time.Now()
	err := c.past(ctx, now, timeout)
	c.m.job("past", start, err)
	return err
}

func (c *compactor) past(ctx context.Context, now time.Time, timeout time.Duration) error {
	var errs []error
	lctx, cancel := context.WithTimeout(ctx, timeout)
	if err := c.sweep(lctx, now, timeout); err != nil {
		c.log.Error("sweep", "err", err)
		errs = append(errs, err)
	}
	objs, err := c.raw.List(lctx, c.l.rawPrefix())
	cancel()
	if err != nil {
		c.log.Error("past", "err", err)
		return errors.Join(append(errs, err)...)
	}
	byHour := map[time.Time][]Object{}
	for _, o := range objs {
		if h, ok := c.l.rawHour(o.Key); ok {
			byHour[h] = append(byHour[h], o)
		}
	}
	hours := make([]time.Time, 0, len(byHour))
	for h := range byHour {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
	cur := now.UTC().Truncate(time.Hour)
	listed := map[time.Time]bool{}
	for _, h := range hours {
		listed[h] = true
		// the listing's count, until a fold of the hour, which deletes,
		// sets it from its own
		c.m.rawHour(h, summarize(byHour[h], nil))
	}
	c.m.rawRetain(listed)
	for _, h := range hours {
		if h.Equal(cur) {
			continue
		}
		_, err := c.foldJob(ctx, h, byHour[h], now, timeout)
		errs = append(errs, err)
	}

	// every past day with hourly sidecars: a no-op unless one of its hours
	// has keys the daily sidecar lacks
	lctx, cancel = context.WithTimeout(ctx, timeout)
	sidecars, err := c.tiers.List(lctx, c.l.hourlySidecarPrefix())
	cancel()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	days := map[time.Time]bool{}
	for _, o := range sidecars {
		if h, ok := sidecarHour(o.Key); ok && dayOf(h).Before(dayOf(now)) {
			days[dayOf(h)] = true
		}
	}
	sorted := make([]time.Time, 0, len(days))
	for d := range days {
		sorted = append(sorted, d)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	for _, d := range sorted {
		_, err := c.rollupJob(ctx, d, timeout)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
