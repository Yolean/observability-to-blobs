package main

import (
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestWorkloadOf(t *testing.T) {
	created := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)
	minute := created.Unix() / 60
	cronJob := "report-" + strconv.FormatInt(minute, 10)
	for _, tc := range []struct {
		what   string
		pod    string
		labels map[string]string
		want   workload
	}{
		{"a Deployment's pod", "web-5d8f7c9b4f-x2x7k", map[string]string{"pod-template-hash": "5d8f7c9b4f", "app": "web"},
			workload{deployment: "web", replicaset: "web-5d8f7c9b4f"}},
		{"a hash shorter than 10 characters", "api-v2-7c4-abcde", map[string]string{"pod-template-hash": "7c4"},
			workload{deployment: "api-v2", replicaset: "api-v2-7c4"}},
		{"a cut generated name: no ReplicaSet", "a-very-long-deployment-name-that-was-truncated-by-genera-x2x7k", map[string]string{"pod-template-hash": "5d8f7c9b4f"},
			workload{}},
		{"a StatefulSet's pod", "kafka-12", map[string]string{"statefulset.kubernetes.io/pod-name": "kafka-12", "controller-revision-hash": "kafka-6b9c"},
			workload{statefulset: "kafka"}},
		{"a DaemonSet's pod", "node-agent-q8dgk", map[string]string{"controller-revision-hash": "7f9c6d", "pod-template-generation": "3"},
			workload{daemonset: "node-agent"}},
		{"a Job's pod", "migrate-abcde", map[string]string{"batch.kubernetes.io/job-name": "migrate", "job-name": "migrate"},
			workload{job: "migrate"}},
		{"a Job's pod, the legacy label only", "migrate-abcde", map[string]string{"job-name": "migrate"},
			workload{job: "migrate"}},
		{"a CronJob's Job", cronJob + "-abcde", map[string]string{"batch.kubernetes.io/job-name": cronJob},
			workload{job: cronJob, cronjob: "report"}},
		{"a Job named with a date is no CronJob's", "report-20261017-abcde", map[string]string{"batch.kubernetes.io/job-name": "report-20261017"},
			workload{job: "report-20261017"}},
		{"a bare pod", "debug", map[string]string{"run": "debug"}, workload{}},
	} {
		if got := workloadOf(tc.pod, tc.labels, created); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.what, got, tc.want)
		}
	}
}

func TestWorkloadFields(t *testing.T) {
	got := workload{deployment: "web", replicaset: "web-5d8f7c9b4f"}.fields()
	want := map[string]string{"k8s_deployment_name": "web", "k8s_replicaset_name": "web-5d8f7c9b4f"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}
