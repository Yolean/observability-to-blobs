package main

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// The key layout of the tiers (compactor/README.md, "Compaction"). Keys are
// relative to the bucket; a store turns them into paths DuckDB reads.

// The tables of the hourly tier, in the order a fold writes them; pods is
// derived from the other three and written after them.
var foldTables = []string{"logs", "metrics", "podindex"}

// sortOrder is each table's ORDER BY in both tiers: namespace first, so
// row-group statistics prune a namespace or a pod to a few row groups.
var sortOrder = map[string]string{
	"logs":     "namespace, pod, pod_uid, container, restart, time",
	"metrics":  "namespace, name, pod, pod_uid, container, restart, time",
	"podindex": "pod_uid, time",
	"pods":     "namespace, pod, pod_started, pod_uid, container, restart",
}

// rowGroups is the ROW_GROUP_SIZE of the tables that replace partitioning
// with sorting; DuckDB rounds 20000 to 20480.
var rowGroups = map[string]int{"logs": 20000, "metrics": 20000}

const dateLayout = "2006-01-02"

// rawKeyRE matches raw1/{cluster}/{date}/{HH}/{node}/{MMSS}-{uuid}.jsonl.zst;
// the file name is not parsed beyond its suffix.
var rawKeyRE = regexp.MustCompile(`^raw1/([^/]+)/(\d{4}-\d{2}-\d{2})/(\d{2})/[^/]+/[^/]+\.jsonl\.zst$`)

// clusterRE keeps the cluster name a single plain key segment.
var clusterRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

type layout struct{ cluster string }

func (l layout) rawPrefix() string { return "raw1/" + l.cluster + "/" }

func (l layout) rawHourPrefix(h time.Time) string {
	return fmt.Sprintf("raw1/%s/%s/%s/", l.cluster, h.Format(dateLayout), h.Format("15"))
}

// rawHour returns the hour a raw key belongs to, from its path.
func (l layout) rawHour(key string) (time.Time, bool) {
	m := rawKeyRE.FindStringSubmatch(key)
	if m == nil || m[1] != l.cluster {
		return time.Time{}, false
	}
	h, err := time.Parse(dateLayout+"/15", m[2]+"/"+m[3])
	return h, err == nil
}

func (l layout) hourly(table string, h time.Time) string {
	return fmt.Sprintf("hourly/%s/%s/%s/%s.parquet", l.cluster, table, h.Format(dateLayout), h.Format("15"))
}

// hourlySidecar is the hour's folded keys, written last as the commit marker.
func (l layout) hourlySidecar(h time.Time) string {
	return fmt.Sprintf("hourly/%s/_folded/%s/%s.csv", l.cluster, h.Format(dateLayout), h.Format("15"))
}

func (l layout) hourlySidecarPrefix() string { return "hourly/" + l.cluster + "/_folded/" }

func (l layout) hourlySidecarDayPrefix(day time.Time) string {
	return l.hourlySidecarPrefix() + day.Format(dateLayout) + "/"
}

var hourlySidecarRE = regexp.MustCompile(`/_folded/(\d{4}-\d{2}-\d{2})/(\d{2})\.csv$`)

// sidecarHour returns the hour of an hourly sidecar key.
func sidecarHour(key string) (time.Time, bool) {
	m := hourlySidecarRE.FindStringSubmatch(key)
	if m == nil {
		return time.Time{}, false
	}
	h, err := time.Parse(dateLayout+"/15", m[1]+"/"+m[2])
	return h, err == nil
}

// dailyPartitionPrefix holds a table's namespace partitions of one day.
func (l layout) dailyPartitionPrefix(table string, day time.Time) string {
	return fmt.Sprintf("daily/%s/%s/%s/", l.cluster, table, day.Format(dateLayout))
}

// dailyPartition is one namespace's object of logs or metrics, keyed the way
// DuckDB's hive partitioning writes and reads it (the value path-escaped).
func (l layout) dailyPartition(table string, day time.Time, namespace string) string {
	return l.dailyPartitionPrefix(table, day) + "namespace=" + url.PathEscape(namespace) + "/data_0.parquet"
}

var dailyPartitionRE = regexp.MustCompile(`/namespace=([^/]*)/data_0\.parquet$`)

// partitionNamespace returns the namespace of a daily partition key.
func partitionNamespace(key string) (string, bool) {
	m := dailyPartitionRE.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	ns, err := url.PathUnescape(m[1])
	return ns, err == nil
}

// daily is the one object of podindex or pods for a day.
func (l layout) daily(table string, day time.Time) string {
	return fmt.Sprintf("daily/%s/%s/%s.parquet", l.cluster, table, day.Format(dateLayout))
}

func (l layout) dailySidecar(day time.Time) string {
	return fmt.Sprintf("daily/%s/_folded/%s.csv.zst", l.cluster, day.Format(dateLayout))
}
