package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The key layout of the tiers (compactor/README.md, "Compaction"). Keys are
// relative to the bucket; a store turns them into paths DuckDB reads.

// The tables of the hourly tier, in the order a fold writes them; pods is
// derived from logs, metrics and podindex and written after them. apiserver
// is the cluster agent's (K8SAGENT.md), and empty without one.
var foldTables = []string{"logs", "metrics", "podindex", "apiserver"}

// sortOrder is each table's ORDER BY in both tiers: namespace first, so
// row-group statistics prune a namespace or a pod to a few row groups.
var sortOrder = map[string]string{
	"logs":     "namespace, pod, pod_uid, container, restart, time",
	"metrics":  "namespace, name, pod, pod_uid, container, restart, time",
	"podindex": "pod_uid, time",
	"pods":     "namespace, pod, pod_started, pod_uid, container, restart",
	// an object's rows adjacent and in time order: the snapshots of an
	// unchanged object repeat one string, which parquet stores once
	"apiserver": "api_group, resource, namespace, name, time",
}

// rowGroups is the ROW_GROUP_SIZE of the tables that replace partitioning
// with sorting; DuckDB rounds 20000 to 20480.
// apiserver rows are whole objects, kilobytes each, so smaller groups.
var rowGroups = map[string]int{"logs": 20000, "metrics": 20000, "apiserver": 5000}

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

// rawHourOfPrefix returns the hour of an hour's prefix,
// raw1/{cluster}/{date}/{HH}/.
func (l layout) rawHourOfPrefix(prefix string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(prefix, l.rawPrefix())
	if !ok {
		return time.Time{}, false
	}
	h, err := time.Parse(dateLayout+"/15/", rest)
	return h, err == nil
}

// The tier objects' keys are a stem and a suffix: ".parquet" for a plain
// object, ".k-{id}.parquet" for one encrypted with the parquet key of that
// id (compactor/README.md, "Encryption"). An object keeps the name, and so
// the key, it was first written with.

// tierObject is an object of the hourly or the daily tier: its key, and
// the id of the parquet key it is encrypted with, "" when it is plain.
type tierObject struct{ key, keyID string }

// tierName is the key of stem's object encrypted with keyID, or plain.
func tierName(stem, keyID string) tierObject {
	if keyID == "" {
		return tierObject{key: stem + ".parquet"}
	}
	return tierObject{key: stem + ".k-" + keyID + ".parquet", keyID: keyID}
}

// keyIDRE is a parquet key id: a key file's name, and a segment of the
// names of the objects encrypted with it.
var keyIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

var tierSuffixRE = regexp.MustCompile(`^(?:\.k-([a-z0-9][a-z0-9-]{0,31}))?\.parquet$`)

// stemObject returns key as an object of stem, if it is one.
func stemObject(stem, key string) (tierObject, bool) {
	rest, ok := strings.CutPrefix(key, stem)
	if !ok {
		return tierObject{}, false
	}
	m := tierSuffixRE.FindStringSubmatch(rest)
	if m == nil {
		return tierObject{}, false
	}
	return tierObject{key: key, keyID: m[1]}, true
}

var tierKeyIDRE = regexp.MustCompile(`\.k-([a-z0-9][a-z0-9-]{0,31})\.parquet$`)

// tierKeyID returns the parquet key id a tier object's name carries, "" for
// a plain object or another key.
func tierKeyID(key string) string {
	if m := tierKeyIDRE.FindStringSubmatch(key); m != nil {
		return m[1]
	}
	return ""
}

func (l layout) tierPrefixes() []string {
	return []string{"hourly/" + l.cluster + "/", "daily/" + l.cluster + "/"}
}

// hourlyStem is the stem of one table's object of an hour.
func (l layout) hourlyStem(table string, h time.Time) string {
	return fmt.Sprintf("hourly/%s/%s/%s/%s", l.cluster, table, h.Format(dateLayout), h.Format("15"))
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

// dailyPartitionStem is the stem of one namespace's object of logs or
// metrics, keyed the way DuckDB's hive partitioning writes and reads it
// (the value path-escaped).
func (l layout) dailyPartitionStem(table string, day time.Time, namespace string) string {
	return l.dailyPartitionPrefix(table, day) + "namespace=" + url.PathEscape(namespace) + "/data_0"
}

var dailyPartitionRE = regexp.MustCompile(`/namespace=([^/]*)/data_0(?:\.k-[a-z0-9][a-z0-9-]{0,31})?\.parquet$`)

// partitionNamespace returns the namespace of a daily partition key.
func partitionNamespace(key string) (string, bool) {
	m := dailyPartitionRE.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	ns, err := url.PathUnescape(m[1])
	return ns, err == nil
}

// dailyStem is the stem of the one object of podindex, pods or objects for
// a day.
func (l layout) dailyStem(table string, day time.Time) string {
	return fmt.Sprintf("daily/%s/%s/%s", l.cluster, table, day.Format(dateLayout))
}

func (l layout) dailySidecar(day time.Time) string {
	return fmt.Sprintf("daily/%s/_folded/%s.csv.zst", l.cluster, day.Format(dateLayout))
}

// tmpPrefixes hold the tiers' temporary objects: every tier write is a COPY
// to a key here, moved to its own key once it reads back whole. What a
// failed job leaves here, the past pass deletes; an operator can give these
// prefixes a short lifecycle rule too.
func (l layout) tmpPrefixes() []string {
	return []string{"hourly/" + l.cluster + "/_tmp/", "daily/" + l.cluster + "/_tmp/"}
}

// tmp is a new temporary key in key's tier. Its suffix is .tmp, so no glob
// of a tier's *.parquet or *.csv ever matches it.
func (l layout) tmp(key string) string {
	tier, _, _ := strings.Cut(key, "/")
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail
	}
	return tier + "/" + l.cluster + "/_tmp/" + hex.EncodeToString(b[:]) + ".tmp"
}
