package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func metricsRun(ctx context.Context, args []string, o, e *bytes.Buffer) error {
	return runMetrics(ctx, args, o, e, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))
}

func scrape(at, pod string, samples ...map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"table": "metrics", "time": at, "ts": at, "cluster": "c", "node": "n1", "job": "pod",
		"instance": "10.0.0.1:9090", "namespace": "qa", "pod": pod, "pod_uid": pod + "-uid", "container": "app", "restart": 0, "samples": samples}
}

func metricSample(name, typ string, v float64, labels map[string]string) map[string]interface{} {
	return map[string]interface{}{"name": name, "type": typ, "labels": labels, "value": v}
}

// A counter per status code, scraped each minute 09:00..09:09; the 200
// series resets to 0 at 09:05 (a restart) and keeps counting.
func TestMetrics(t *testing.T) {
	dir := t.TempDir()
	var lines []map[string]interface{}
	ok200 := []float64{0, 60, 120, 180, 240, 0, 60, 120, 180, 240}
	for i := 0; i < 10; i++ {
		at := time.Date(2026, 10, 11, 9, i, 0, 0, time.UTC).Format(time.RFC3339)
		lines = append(lines, scrape(at, "web-1",
			metricSample("http_requests_total", "counter", ok200[i], map[string]string{"code": "200"}),
			metricSample("http_requests_total", "counter", float64(6*i), map[string]string{"code": "500"}),
			metricSample("up", "gauge", 1, map[string]string{})))
	}
	lines = append(lines, scrape("2026-10-11T09:09:30Z", "web-2", metricSample("up", "gauge", 0, map[string]string{})))
	writeRaw(t, filepath.Join(dir, "raw1/c/2026-10-11/09/n1/a.jsonl.zst"), lines)
	w := []string{"--to", "2026-10-11 09:10"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"names"}, "name\ttype\tseries\tsamples\tlast\nhttp_requests_total\tcounter\t2\t20\t2026-10-11T09:09:00.000000000Z\nup\tgauge\t2\t11\t2026-10-11T09:09:30.000000000Z\n"},
		// 200: 4 increases of 60, the reset to 0 adds 0, 4 more of 60: 480
		// in 600 s; 500: 9 increases of 6, 54 in 600 s
		{[]string{"rate", "http_requests_total", "--by", "code", "--step", "10m"}, "time\tcode\trate\n2026-10-11T09:00:00.000000000Z\t200\t0.8\n2026-10-11T09:00:00.000000000Z\t500\t0.09\n"},
		{[]string{"series", "http_requests_total", "--where", "code=500", "--step", "5m", "--format", "csv"}, "time,namespace,pod,pod_uid,container,instance,labels,value\n" +
			`2026-10-11T09:00:00.000000000Z,qa,web-1,web-1-uid,app,10.0.0.1:9090,"{""code"":""500""}",24` + "\n" +
			`2026-10-11T09:05:00.000000000Z,qa,web-1,web-1-uid,app,10.0.0.1:9090,"{""code"":""500""}",54` + "\n"},
		{[]string{"latest", "up", "--value", "0", "--format", "csv"}, "time,namespace,pod,pod_uid,container,instance,labels,value\n2026-10-11T09:09:30.000000000Z,qa,web-2,web-2-uid,app,10.0.0.1:9090,{},0\n"},
		{[]string{"series", "up", "--by", "pod", "--agg", "max", "--step", "10m"}, "time\tpod\tvalue\n2026-10-11T09:00:00.000000000Z\tweb-1\t1\n2026-10-11T09:00:00.000000000Z\tweb-2\t0\n"},
	} {
		out, e, err := runCmd(t, metricsRun, dir, append(c.args, w...)...)
		if err != nil || out != c.want {
			t.Errorf("%v: %q %v %s\nwant %q", c.args, out, err, e, c.want)
		}
	}
	if _, _, err := runCmd(t, metricsRun, dir, "series", "http_request_total"); exitOf(err) != exitNotFound || !strings.Contains(err.Error(), "closest names: http_requests_total") {
		t.Errorf("unknown metric: %v", err)
	}
	for _, args := range [][]string{{}, {"plot"}, {"rate"}, {"names", "x"}, {"rate", "x", "--agg", "median"}, {"series", "x", "--by", "bad key"}} {
		var o, e bytes.Buffer
		if err := metricsRun(context.Background(), args, &o, &e); exitOf(err) != exitUsage {
			t.Errorf("%v: %v", args, err)
		}
	}
}
