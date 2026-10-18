package main

import (
	"context"
	"errors"
	"fmt"
	"net"
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
//   - a rollup of a past day runs after the rolling pass that committed
//     the final fold of that day's last hour, and in every past pass for
//     every past day with hourly sidecars, where it does nothing unless an
//     hour has keys the daily sidecar lacks; between its writes the
//     current hour of every cluster folds when ROLLING_INTERVAL is due;
//   - at start and on every past pass, checkKeys: while a parquet key that
//     a tier object's name carries has no key file, both passes refuse to
//     fold, and the rolling pass checks again on every tick.
//
// Nothing is remembered between ticks that the bucket does not say, so a
// start is an ordinary tick. One writer: replicas 1, strategy Recreate.
//
// Without CLUSTER_NAME every pass runs for each cluster in turn
// (clusters.go): the rolling pass first lists raw1/ for clusters new
// since the last tick, and the past pass raw1/ and hourly/.
//
// The metrics, put and logs listeners are bound before the first pass, so that
// an address that cannot be bound ends the run at once, as does a
// listener that fails later.
func (c *compactor) run(ctx context.Context, cfg config) error {
	// Between a rollup's writes every cluster's current hour folds when
	// ROLLING_INTERVAL has passed since the last time it did: the rollup
	// holds back no rolling fold by more than one of its writes.
	var f *fleet
	var lastRolling time.Time
	c.interleave = func() {
		if time.Since(lastRolling) < cfg.rolling {
			return
		}
		lastRolling = time.Now()
		f.each(ctx, func(c *compactor) { c.currentPass(ctx, lastRolling.UTC(), cfg.jobTimeout) })
	}
	f = newFleet(c, cfg.cluster)
	srv := &http.Server{Addr: cfg.metricsAddr, Handler: f.set, ReadHeaderTimeout: 10 * time.Second}
	ml, err := net.Listen("tcp", cfg.metricsAddr)
	if err != nil {
		return fmt.Errorf("METRICS_ADDR: %w", err)
	}
	srvErr := make(chan error, 3)
	go func() { srvErr <- fmt.Errorf("METRICS_ADDR: %w", srv.Serve(ml)) }()
	defer srv.Close()
	if cfg.putListen != "" {
		put, err := c.putServer(cfg)
		if err != nil {
			return err
		}
		pl, err := net.Listen("tcp", cfg.putListen)
		if err != nil {
			return fmt.Errorf("PUT_LISTEN: %w", err)
		}
		go func() { srvErr <- fmt.Errorf("PUT_LISTEN: %w", put.Serve(pl)) }()
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
		ll, err := net.Listen("tcp", cfg.logsListen)
		if err != nil {
			return fmt.Errorf("LOGS_LISTEN: %w", err)
		}
		logs := logsServer(cfg.logsListen, newLogsHandler(r, cfg.logsTimeout, cfg.logsConcurrency, c.log))
		go func() { srvErr <- fmt.Errorf("LOGS_LISTEN: %w", logs.Serve(ll)) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logs.Shutdown(sctx)
		}()
	}
	cluster := cfg.cluster
	if cluster == "" {
		cluster = "every cluster in the bucket"
	}
	c.log.Info("run", "cluster", cluster, "rolling", cfg.rolling.String(), "grace", cfg.grace.String(),
		"past", cfg.past.String(), "raw_delete", cfg.rawDelete, "metrics", cfg.metricsAddr,
		"raw_source", cfg.rawSource, "put", cfg.putListen, "logs", cfg.logsListen,
		"parquet_key", c.keyID, "parquet_keys", sortedIDs(c.keys))
	if cfg.cluster != "" {
		c.keysJob(jobContext(ctx), cfg.jobTimeout)
	}
	tick := time.NewTicker(cfg.rolling)
	defer tick.Stop()
	var lastPast time.Time
	for {
		now := time.Now().UTC()
		past := lastPast.IsZero() || now.Sub(lastPast) >= cfg.past
		f.discover(ctx, past, cfg.jobTimeout)
		lastRolling = time.Now()
		f.each(ctx, func(c *compactor) { c.rollingPass(ctx, now, cfg.jobTimeout) })
		if ctx.Err() == nil && past {
			f.each(ctx, func(c *compactor) { c.pastPass(ctx, now, cfg.jobTimeout) })
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
	c.foldedKeys = newFoldedKeys(c.readHourFolded)
	h := &putServer{l: c.l, bucket: cfg.bucket, region: cfg.region, keyID: cfg.putKeyID, secret: cfg.putKey,
		maxBytes: cfg.putMaxBytes, spool: sp, folded: c.foldedKeys, m: c.m, log: c.log, now: time.Now}
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
		c.guardLog(err)
		c.log.Error("fold", "hour", hour, "err", err)
		return r, err
	}
	c.m.fold(r)
	if r.wrote || len(r.deleted) > 0 {
		c.log.Info("fold", "hour", hour, "keys", r.keys, "full", r.full, "final", r.final, "deleted", len(r.deleted),
			"logs", r.rows["logs"], "metrics", r.rows["metrics"], "podindex", r.rows["podindex"], "pods", r.rows["pods"], "apiserver", r.rows["apiserver"],
			"runs", r.runs, "seconds", time.Since(start).Seconds())
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
		c.guardLog(err)
		c.log.Error("rollup", "day", day.Format(dateLayout), "err", err)
		return r, err
	}
	c.m.rollup(r)
	if len(r.hours) > 0 {
		c.log.Info("rollup", "day", day.Format(dateLayout), "hours", len(r.hours), "namespaces", r.namespaces,
			"logs", r.rows["logs"], "metrics", r.rows["metrics"], "podindex", r.rows["podindex"], "pods", r.rows["pods"], "apiserver", r.rows["apiserver"],
			"runs", r.runs, "seconds", time.Since(start).Seconds())
	}
	return r, nil
}

// guardLog counts a refusal (guard.go) and logs it with its parts as
// attributes: the job, the hour or day, the count and a key that has one.
func (c *compactor) guardLog(err error) {
	var e *unaccountedError
	if !errors.As(err, &e) {
		return
	}
	c.m.refused(e)
	attrs := []any{"job", e.job, "period", e.period, "count", e.n, "sample_key", e.sample}
	if e.fields != "" {
		attrs = append(attrs, "fields", e.fields)
	}
	c.log.Error("unaccounted", attrs...)
}

// keysJob is checkKeys with a deadline; it logs and sets the refusal.
func (c *compactor) keysJob(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.checkKeys(ctx)
}

// rollingPass folds the current hour and, until it is final, the previous.
// While a parquet key is missing it checks the keys again instead. When
// the previous hour's final fold ends a day, the day's first rollup runs
// after the pass, which it does not count in.
func (c *compactor) rollingPass(ctx context.Context, now time.Time, timeout time.Duration) error {
	start := time.Now()
	day, err := c.rolling(ctx, now, timeout)
	c.m.job("rolling", start, err)
	if day.IsZero() {
		return err
	}
	if serr := stopped(ctx); serr != nil {
		return errors.Join(err, serr)
	}
	_, rerr := c.rollupJob(jobContext(ctx), day, timeout)
	return errors.Join(err, rerr)
}

// currentPass is the rolling pass's fold of the current hour alone, which
// counts as a rolling pass. A rollup runs it between its partitions
// (interleave), so that a long rollup does not hold the current hour back.
func (c *compactor) currentPass(ctx context.Context, now time.Time, timeout time.Duration) {
	if c.refusal != nil || stopped(ctx) != nil {
		return
	}
	start := time.Now()
	_, err := c.foldJob(jobContext(ctx), now.UTC().Truncate(time.Hour), nil, now, timeout)
	c.m.job("rolling", start, err)
}

// rolling is the rolling pass's folds; day is the day to roll up after
// them, zero for none.
func (c *compactor) rolling(ctx context.Context, now time.Time, timeout time.Duration) (day time.Time, err error) {
	jobs := jobContext(ctx)
	cur := now.UTC().Truncate(time.Hour)
	prev := cur.Add(-time.Hour)
	if err := stopped(ctx); err != nil {
		return day, err
	}
	if c.refusal != nil {
		if err := c.keysJob(jobs, timeout); err != nil {
			return day, err
		}
	}
	_, err = c.foldJob(jobs, cur, nil, now, timeout)
	errs := []error{err}
	if prev.After(c.finalSeen) {
		if err := stopped(ctx); err != nil {
			return day, errors.Join(append(errs, err)...)
		}
		r, err := c.foldJob(jobs, prev, nil, now, timeout)
		errs = append(errs, err)
		if err == nil && r.final {
			c.finalSeen = prev
			// the day's last hour just became final: the day's first
			// rollup; after a stop the next past pass makes it
			if r.wrote && r.full && dayOf(prev).Before(dayOf(cur)) {
				day = dayOf(prev)
			}
		}
	}
	return day, errors.Join(errs...)
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
	// has keys the daily sidecar lacks. A no-op logs nothing of its own
	// and counts as a rollup success; one line per pass says how many
	// days were checked and how many rolled up.
	var days []string
	if err := list(func(ctx context.Context) (err error) {
		days, err = c.tiers.Dirs(ctx, c.l.hourlySidecarPrefix())
		return
	}); err != nil {
		fail("list", err)
		return errors.Join(errs...)
	}
	checked, rolled := 0, 0
	defer func() { c.log.Info("rollups", "days", checked, "rolled_up", rolled) }()
	for _, dp := range days {
		d, err := time.Parse(dateLayout+"/", strings.TrimPrefix(dp, c.l.hourlySidecarPrefix()))
		if err != nil || !d.Before(dayOf(now)) {
			continue
		}
		if err := stopped(ctx); err != nil {
			return errors.Join(append(errs, err)...)
		}
		r, err := c.rollupJob(jobs, d, timeout)
		errs = append(errs, err)
		checked++
		if len(r.hours) > 0 {
			rolled++
		}
	}
	return errors.Join(errs...)
}
