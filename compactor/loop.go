package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The loop's cadence (compactor/README.md, "Compaction", decision 3):
//
//   - every ROLLING_INTERVAL, the rolling pass folds the current hour, and
//     the previous hour until its final fold has committed;
//   - every PAST_INTERVAL, and on the first tick after a start, the past
//     pass lists raw1/{cluster}/ hour by hour and folds every other hour
//     with keys its sidecar lacks, or that is past H+1h+GRACE and not
//     final yet;
//   - a rollup of a past day runs after the rolling pass committed the
//     final fold of that day's last hour, and after every past pass for
//     every past day with hourly sidecars, where it does nothing unless an
//     hour has keys the daily sidecar lacks;
//   - at start and on every past pass, checkKeys: while a parquet key that
//     a tier object's name carries has no key file, both passes refuse to
//     fold, and the rolling pass checks again on every tick.
//
// Nothing is remembered between ticks that the bucket does not say, so a
// start is an ordinary tick. One writer: replicas 1, strategy Recreate.
func (c *compactor) run(ctx context.Context, cfg config) error {
	srv := &http.Server{Addr: cfg.metricsAddr, Handler: c.m, ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 3)
	go func() { srvErr <- srv.ListenAndServe() }()
	defer srv.Close()
	if cfg.putListen != "" {
		put, err := c.putServer(cfg)
		if err != nil {
			return err
		}
		go func() { srvErr <- put.ListenAndServe() }()
		// a stop lets the uploads in flight finish: each is answered
		// only once it is synced
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			put.Shutdown(sctx)
		}()
	}
	if cfg.logsListen != "" {
		r, err := newLogsReader(cfg, c.raw, c.tiers, c.log)
		if err != nil {
			return err
		}
		defer r.Close()
		logs := logsServer(cfg.logsListen, newLogsHandler(r, cfg.logsTimeout, cfg.logsConcurrency, c.log))
		go func() { srvErr <- logs.ListenAndServe() }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logs.Shutdown(sctx)
		}()
	}
	c.log.Info("run", "cluster", c.l.cluster, "rolling", cfg.rolling.String(), "grace", cfg.grace.String(),
		"past", cfg.past.String(), "raw_delete", cfg.rawDelete, "metrics", cfg.metricsAddr,
		"raw_source", cfg.rawSource, "put", cfg.putListen, "logs", cfg.logsListen,
		"parquet_key", c.keyID, "parquet_keys", sortedIDs(c.keys))
	c.keysJob(jobContext(ctx), cfg.jobTimeout)
	tick := time.NewTicker(cfg.rolling)
	defer tick.Stop()
	var lastPast time.Time
	for {
		now := time.Now().UTC()
		c.rollingPass(ctx, now, cfg.jobTimeout)
		if ctx.Err() == nil && (lastPast.IsZero() || now.Sub(lastPast) >= cfg.past) {
			c.pastPass(ctx, now, cfg.jobTimeout)
			lastPast = now
		}
		select {
		case <-ctx.Done():
			c.log.Info("run", "stopped", true)
			return nil
		case err := <-srvErr:
			return err
		case <-tick.C:
		}
	}
}

// putServer is the put endpoint's server, which stores into the spool
// the fold reads.
func (c *compactor) putServer(cfg config) (*http.Server, error) {
	sp, ok := c.raw.(*spool)
	if !ok {
		return nil, errors.New("PUT_LISTEN: the raw source is not a spool")
	}
	h := &putServer{l: c.l, bucket: cfg.bucket, region: cfg.region, keyID: cfg.putKeyID, secret: cfg.putKey,
		maxBytes: cfg.putMaxBytes, spool: sp, m: c.m, log: c.log, now: time.Now}
	return &http.Server{Addr: cfg.putListen, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}, nil
}

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// A stop (SIGTERM, ctx done) ends a pass between its jobs: the job in
// flight finishes, under its own deadline (jobContext), rather than leave
// objects ahead of their sidecar, and no other starts. So a stop takes at
// most one job, which terminationGracePeriodSeconds allows for, and a
// stopped compactor leaves a quiet bucket.
var errStopped = errors.New("stopped between jobs")

func stopped(ctx context.Context) error {
	if ctx.Err() != nil {
		return errStopped
	}
	return nil
}

// jobContext is a pass's context for its jobs: not cancelled by a stop.
func jobContext(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

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

// keysJob is checkKeys with a deadline; it logs and sets the refusal.
func (c *compactor) keysJob(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.checkKeys(ctx)
}

// rollingPass folds the current hour and, until it is final, the previous.
// While a parquet key is missing it checks the keys again instead.
func (c *compactor) rollingPass(ctx context.Context, now time.Time, timeout time.Duration) error {
	start := time.Now()
	err := c.rolling(ctx, now, timeout)
	c.m.job("rolling", start, err)
	return err
}

func (c *compactor) rolling(ctx context.Context, now time.Time, timeout time.Duration) error {
	jobs := jobContext(ctx)
	cur := now.UTC().Truncate(time.Hour)
	prev := cur.Add(-time.Hour)
	if err := stopped(ctx); err != nil {
		return err
	}
	if c.refusal != nil {
		if err := c.keysJob(jobs, timeout); err != nil {
			return err
		}
	}
	_, err := c.foldJob(jobs, cur, nil, now, timeout)
	errs := []error{err}
	if prev.After(c.finalSeen) {
		if err := stopped(ctx); err != nil {
			return errors.Join(append(errs, err)...)
		}
		r, err := c.foldJob(jobs, prev, nil, now, timeout)
		errs = append(errs, err)
		if err == nil && r.final {
			c.finalSeen = prev
			// the day's last hour just became final: the day's first
			// rollup; after a stop the next past pass makes it
			if r.wrote && r.full && dayOf(prev).Before(dayOf(cur)) {
				if err := stopped(ctx); err != nil {
					return errors.Join(append(errs, err)...)
				}
				_, err := c.rollupJob(jobs, dayOf(prev), timeout)
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// pastPass deletes abandoned temporary objects, lists the cluster's raw
// tier hour by hour, folds every hour but the current one that has new keys
// or is due its final fold, sets the raw gauges from those listings, and
// rolls up every past day.
func (c *compactor) pastPass(ctx context.Context, now time.Time, timeout time.Duration) error {
	start := time.Now()
	err := c.past(ctx, now, timeout)
	c.m.job("past", start, err)
	return err
}

// past holds one hour's listing at a time: the dates under raw1/{cluster}/
// and the hours under each come from listings with a delimiter, one entry
// per prefix, so memory is bounded by the largest hour, not the tier.
func (c *compactor) past(ctx context.Context, now time.Time, timeout time.Duration) error {
	jobs := jobContext(ctx)
	var errs []error
	fail := func(what string, err error) {
		c.log.Error("past", "step", what, "err", err)
		errs = append(errs, err)
	}
	list := func(f func(context.Context) error) error {
		lctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return f(lctx)
	}
	if err := c.keysJob(jobs, timeout); err != nil {
		return err
	}
	if err := list(func(ctx context.Context) error { return c.sweep(ctx, now, timeout) }); err != nil {
		fail("sweep", err)
	}
	var dates []string
	if err := list(func(ctx context.Context) (err error) { dates, err = c.raw.Dirs(ctx, c.l.rawPrefix()); return }); err != nil {
		fail("list", err)
		return errors.Join(errs...)
	}
	cur := now.UTC().Truncate(time.Hour)
	seen := map[time.Time]bool{}
	complete := true
	for _, date := range dates {
		var hours []string
		if err := list(func(ctx context.Context) (err error) { hours, err = c.raw.Dirs(ctx, date); return }); err != nil {
			fail("list", err)
			complete = false
			continue
		}
		for _, hp := range hours {
			h, ok := c.l.rawHourOfPrefix(hp)
			if !ok {
				continue
			}
			if err := stopped(ctx); err != nil {
				return errors.Join(append(errs, err)...)
			}
			var listed []Object
			if err := list(func(ctx context.Context) error {
				objs, err := c.raw.List(ctx, hp)
				for _, o := range objs {
					if oh, ok := c.l.rawHour(o.Key); ok && oh.Equal(h) {
						listed = append(listed, o)
					}
				}
				return err
			}); err != nil {
				fail("list", err)
				complete = false
				continue
			}
			seen[h] = true
			// the listing's count, until the fold, which deletes, sets its own
			c.m.rawHour(h, summarize(listed, nil))
			if h.Equal(cur) {
				continue
			}
			_, err := c.foldJob(jobs, h, listed, now, timeout)
			errs = append(errs, err)
		}
	}
	if complete {
		c.m.rawRetain(seen)
	}

	// every past day with hourly sidecars: a no-op unless one of its hours
	// has keys the daily sidecar lacks
	var days []string
	if err := list(func(ctx context.Context) (err error) {
		days, err = c.tiers.Dirs(ctx, c.l.hourlySidecarPrefix())
		return
	}); err != nil {
		fail("list", err)
		return errors.Join(errs...)
	}
	for _, dp := range days {
		d, err := time.Parse(dateLayout+"/", strings.TrimPrefix(dp, c.l.hourlySidecarPrefix()))
		if err != nil || !d.Before(dayOf(now)) {
			continue
		}
		if err := stopped(ctx); err != nil {
			return errors.Join(append(errs, err)...)
		}
		_, err = c.rollupJob(jobs, d, timeout)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
