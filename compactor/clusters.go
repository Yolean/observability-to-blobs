package main

import (
	"context"
	"sort"
	"strings"
	"time"
)

// A run without CLUSTER_NAME compacts every cluster of the bucket: those
// with a prefix under raw1/, what has arrived, and under hourly/, what has
// been folded, so that a cluster whose raw objects are all folded and
// deleted still gets its last rollups. The tiers stay the only state: a
// start finds every cluster again. Each cluster has a compactor of its own
// (forCluster) on the one engine, and the passes go cluster by cluster. A
// failed job is logged and its pass goes on, so a cluster that fails costs
// the others the time of its jobs, at most JOB_TIMEOUT each, and nothing
// else.
type fleet struct {
	base   *compactor
	fixed  string // CLUSTER_NAME; "" for every cluster
	set    *metricsSet
	byName map[string]*compactor
	names  []string // sorted
}

func newFleet(c *compactor, cluster string) *fleet {
	f := &fleet{base: c, fixed: cluster, set: &metricsSet{}, byName: map[string]*compactor{}}
	if cluster != "" {
		f.byName[cluster] = c
		f.names = []string{cluster}
		f.set.add(c.m)
	}
	return f
}

// forCluster is c for another cluster: the same engine, stores and keys,
// with its own layout, metrics and the caches its passes keep.
func (c *compactor) forCluster(name string) *compactor {
	cp := *c
	cp.l = layout{name}
	cp.m = newMetrics(name)
	cp.log = c.log.With("cluster", name)
	cp.finalSeen = time.Time{}
	cp.refusal = nil
	cp.foldedKeys = nil
	return &cp
}

// dirLister is what RawSource and TierStore have in common.
type dirLister interface {
	Dirs(ctx context.Context, prefix string) ([]string, error)
}

// clusterDirs is the cluster names one level under root ("raw1/" or
// "hourly/"); other prefixes there are not clusters and are left out.
func clusterDirs(ctx context.Context, s dirLister, root string) ([]string, error) {
	dirs, err := s.Dirs(ctx, root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if name := strings.TrimSuffix(strings.TrimPrefix(d, root), "/"); clusterRE.MatchString(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// discover adds the clusters with a prefix under raw1/ and, with tiers,
// under hourly/. A cluster's parquet keys are checked before its first
// pass. With CLUSTER_NAME it does nothing. Clusters are never dropped: one
// that stops writing keeps its passes, which find nothing to do.
func (f *fleet) discover(ctx context.Context, tiers bool, timeout time.Duration) error {
	if f.fixed != "" {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	found, err := clusterDirs(lctx, f.base.raw, "raw1/")
	if tiers && err == nil {
		var folded []string
		folded, err = clusterDirs(lctx, f.base.tiers, "hourly/")
		found = append(found, folded...)
	}
	for _, name := range found {
		if _, ok := f.byName[name]; ok {
			continue
		}
		c := f.base.forCluster(name)
		f.byName[name] = c
		f.names = append(f.names, name)
		sort.Strings(f.names)
		f.set.add(c.m)
		c.log.Info("cluster found")
		c.keysJob(jobContext(ctx), timeout)
	}
	if err != nil {
		f.base.log.Error("clusters", "err", err)
	}
	return err
}

// each runs pass for every cluster in name order, until ctx is done.
func (f *fleet) each(ctx context.Context, pass func(c *compactor)) {
	for _, name := range f.names {
		if ctx.Err() != nil {
			return
		}
		pass(f.byName[name])
	}
}
