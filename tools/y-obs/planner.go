package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// finalAfter is how long after its end an hour's hourly object is final:
// the compactor's GRACE, at most 30m (compactor/README.md, "The loop").
// Before that the raw tier is the complete one.
const finalAfter = 30 * time.Minute

// The window views w_<table> read every hour of the window from exactly
// one tier, so no row is read twice:
//
//   - an hour that is final and has hourly objects for every table asked
//     for reads the hourly tier;
//   - else an hour with raw objects reads them (the current hours, and any
//     hour whose fold is behind);
//   - else an hour with hourly objects reads those;
//   - else its day, when the daily tier has it, reads the daily objects,
//     and then every hour of that day does, since a daily object holds the
//     whole day.
//
// Rows are then cut to the window by time. The hour each tier serves is
// in s.sources, for the summary line.

type hourSource string

const (
	fromHourly hourSource = "hourly"
	fromRaw    hourSource = "raw"
	fromDaily  hourSource = "daily"
	fromNone   hourSource = "none"
)

// windowPlan is what planWindow decided.
type windowPlan struct {
	tables  []string
	hourly  map[string][]string // table -> hourly objects of the hours that read them
	daily   map[string][]string // table -> daily objects of the days that read them
	raw     []string            // raw objects of the hours that read them
	sources map[string]hourSource
}

// planWindow plans tables (of tierTables) over the session's window. ns,
// when set, prunes the daily tier's namespace partitions.
func (s *session) planWindow(ctx context.Context, tables []string, ns string, now time.Time) error {
	p := &windowPlan{tables: tables, hourly: map[string][]string{}, daily: map[string][]string{}, sources: map[string]hourSource{}}
	// the hourly objects of every day, by table and hour
	hourlyBy := map[string]map[string][]string{}
	for _, t := range tables {
		hourlyBy[t] = map[string][]string{}
		for _, d := range s.w.days() {
			files, err := s.glob(ctx, fmt.Sprintf("%s/hourly/%s/%s/%s/*.parquet", s.root, s.cluster, t, d))
			if err != nil {
				return err
			}
			for _, f := range s.readable("hourly_"+t, files) {
				if m := hourlyName.FindStringSubmatch(f); m != nil {
					hourlyBy[t][d+"/"+m[1]] = append(hourlyBy[t][d+"/"+m[1]], f)
				}
			}
		}
	}
	dailyDays := map[string]bool{}
	for _, h := range s.w.hours() {
		key := h.Format("2006-01-02/15")
		all, any := true, false
		for _, t := range tables {
			has := len(hourlyBy[t][key]) > 0
			all, any = all && has, any || has
		}
		if all && h.Add(time.Hour+finalAfter).Before(now) {
			p.sources[key] = fromHourly
			continue
		}
		raw, err := s.rawHour(ctx, h)
		if err != nil {
			return err
		}
		switch {
		case len(raw) > 0:
			p.sources[key] = fromRaw
			p.raw = append(p.raw, raw...)
		case any:
			p.sources[key] = fromHourly
		default:
			p.sources[key] = fromNone
			dailyDays[h.Format("2006-01-02")] = true
		}
	}
	// a window that starts within rawBefore of an hour also takes the
	// previous hour's objects opened in that margin: the hour's last object
	// holds the window's first rows
	var margin []string
	if first := s.w.from.Truncate(time.Hour); s.w.from.Add(-rawBefore).Before(first) && p.sources[first.Format("2006-01-02/15")] == fromRaw {
		fs, err := s.rawHour(ctx, first.Add(-time.Hour))
		if err != nil {
			return err
		}
		margin = fs
	}
	// a finished day with an hour no other tier has reads the daily tier,
	// if it has the day, for all its hours; a day not yet finished has at
	// most a partial rollup, and keeps its hours' sources
	for d := range dailyDays {
		day, _ := time.Parse("2006-01-02", d)
		if !day.Add(24*time.Hour + finalAfter).Before(now) {
			continue
		}
		found := false
		for _, t := range tables {
			files, err := s.dailyObjects(ctx, t, d, ns)
			if err != nil {
				return err
			}
			files = s.readable("daily_"+t, files)
			if len(files) > 0 {
				found = true
				p.daily[t] = append(p.daily[t], files...)
			}
		}
		if !found {
			continue
		}
		for key := range p.sources {
			if strings.HasPrefix(key, d+"/") {
				p.sources[key] = fromDaily
			}
		}
	}
	// the raw objects of hours the daily tier now serves are not read
	rawHours := map[string]bool{}
	for key, src := range p.sources {
		if src == fromRaw {
			rawHours[key] = true
		}
	}
	kept := p.raw[:0]
	for _, f := range p.raw {
		if rawHours[rawHourOf(f)] {
			kept = append(kept, f)
		}
	}
	p.raw = append(kept, margin...)
	for _, t := range tables {
		for key, src := range p.sources {
			if src == fromHourly {
				p.hourly[t] = append(p.hourly[t], hourlyBy[t][key]...)
			}
		}
		sort.Strings(p.hourly[t])
	}
	s.wplan = p
	s.plan["raw"] = p.raw
	for _, t := range tables {
		if len(p.hourly[t]) > 0 {
			s.plan["hourly_"+t] = p.hourly[t]
		}
		if len(p.daily[t]) > 0 {
			s.plan["daily_"+t] = p.daily[t]
		}
	}
	return nil
}

// readable drops the objects under a parquet key the session lacks, with
// a warning: a view cannot read them, and a plan that named them would
// fail the query.
func (s *session) readable(view string, files []string) []string {
	var keep []string
	missing := map[string]int{}
	for _, f := range files {
		if id := keyOf(f); id != "" && !s.keyIDs[id] {
			missing[id]++
			continue
		}
		keep = append(keep, f)
	}
	for id, n := range missing {
		s.warnings = append(s.warnings, fmt.Sprintf("%s: %d objects under parquet key %s are left out: --parquet-keys-dir lacks it", view, n, id))
	}
	return keep
}

// rawHour lists one hour's raw objects, of one node with --node.
func (s *session) rawHour(ctx context.Context, h time.Time) ([]string, error) {
	node := "*"
	if s.node != "" {
		node = s.node
	}
	files, err := s.glob(ctx, fmt.Sprintf("%s/raw1/%s/%s/%s/*.jsonl.zst", s.root, s.cluster, h.Format("2006-01-02/15"), node))
	return s.rawInWindow(files), err
}

// rawHourOf is the date/HH of a raw key: raw1/{cluster}/{date}/{HH}/...
func rawHourOf(f string) string {
	parts := strings.Split(f, "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "raw1" {
			return parts[i+2] + "/" + parts[i+3]
		}
	}
	return ""
}

// dailyObjects lists one table's daily objects of a day; logs and metrics
// only the namespace's partition when ns is set.
func (s *session) dailyObjects(ctx context.Context, table, day, ns string) ([]string, error) {
	switch {
	case dailyPartitioned(table) && ns != "":
		return s.glob(ctx, fmt.Sprintf("%s/daily/%s/%s/%s/namespace=%s/*.parquet", s.root, s.cluster, table, day, ns))
	case dailyPartitioned(table):
		return s.glob(ctx, fmt.Sprintf("%s/daily/%s/%s/%s/*/*.parquet", s.root, s.cluster, table, day))
	}
	return s.glob(ctx, fmt.Sprintf("%s/daily/%s/%s/%s*.parquet", s.root, s.cluster, table, day))
}

// createWindowViews creates w_<table> for the planned tables: the hourly
// and daily objects and the raw-derived view, united by name, cut to the
// window by time (pods has no time column: its rows are incarnations
// seen in the hours read). The raw-derived view is always among them, over
// an empty file when no raw is planned, so that every column of this raw
// contract exists, also over objects a compactor of an earlier one wrote.
func (s *session) createWindowViews(ctx context.Context) error {
	if err := s.createViews(ctx); err != nil {
		return err
	}
	from := sqlString(s.w.from.Format("2006-01-02 15:04:05.000000000"))
	to := sqlString(s.w.to.Format("2006-01-02 15:04:05.000000000"))
	for _, t := range s.wplan.tables {
		var parts []string
		for _, tier := range []string{"hourly_", "daily_"} {
			if len(s.plan[tier+t]) > 0 {
				parts = append(parts, "SELECT * FROM "+tier+t)
			}
		}
		parts = append(parts, "SELECT * FROM "+t)
		q := strings.Join(parts, " UNION ALL BY NAME ")
		if t != "pods" {
			q = "SELECT * FROM (" + q + ") WHERE time >= TIMESTAMP_NS " + from + " AND time < TIMESTAMP_NS " + to
		}
		if err := s.exec(ctx, "CREATE OR REPLACE VIEW w_"+t+" AS "+q); err != nil {
			return err
		}
	}
	return nil
}

// sourcesSummary is the hours per tier: "hourly=5 raw=2".
func (s *session) sourcesSummary() string {
	if s.wplan == nil {
		return ""
	}
	n := map[hourSource]int{}
	for _, src := range s.wplan.sources {
		n[src]++
	}
	var parts []string
	for _, src := range []hourSource{fromRaw, fromHourly, fromDaily, fromNone} {
		if n[src] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", src, n[src]))
		}
	}
	return "hours " + strings.Join(parts, " ")
}
