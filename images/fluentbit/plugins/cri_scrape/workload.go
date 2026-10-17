package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// workload is a pod's controllers as the OpenTelemetry semantic
// conventions name them (k8s.deployment.name and so on, all stable), each
// "" when the pod has none of that kind.
//
// The k8sattributes processor of the OpenTelemetry Collector
// (processor/k8sattributesprocessor, internal/kube/client.go, at
// v0.163.0) reads them from the pod's ownerReferences, and derives the
// Deployment from the ReplicaSet's name and the CronJob from the Job's.
// The container runtime has no ownerReferences, so here the owners come
// from the labels their controllers put on every pod they create, and the
// names from the pod's; the Deployment and the CronJob then follow by the
// processor's own rules. Where the result can be wrong:
//
//   - A Job whose own name ends in -<8 digits> within a day of the pod's
//     creation, as a CronJob's Jobs do, is taken for a CronJob's (the
//     processor has the same rule, and the same case).
//   - A ReplicaSet whose Deployment is gone (deleted with
//     --cascade=orphan) still names that Deployment.
//   - Controllers outside Kubernetes (Argo Rollouts, OpenKruise and others)
//     name nothing, or with controller-revision-hash look like a DaemonSet.
//   - A pod whose generated name was cut short (an owner name over 58
//     characters) has no ReplicaSet or DaemonSet; static pods have none.
type workload struct {
	deployment, replicaset, statefulset, daemonset, job, cronjob string
}

// cronJobSuffix is the processor's: a CronJob names its Jobs
// <cronjob>-<scheduled time in unix minutes>.
var (
	cronJobSuffix = regexp.MustCompile(`^(.*)-(\d{8})$`)
	ordinal       = regexp.MustCompile(`^(.+)-\d+$`)
)

// cronJobSkew is the processor's bound between a Job's scheduled minute
// and its pod's creation, so that a Job named with a date is no CronJob's.
const cronJobSkew = 24 * time.Hour

// workloadOf derives the workload of a pod from its name, its labels and
// its sandbox's creation.
func workloadOf(pod string, labels map[string]string, created time.Time) workload {
	var w workload
	// trimmed is the pod's name without the random suffix its owner's
	// generateName gave it
	trimmed := ""
	if i := strings.LastIndex(pod, "-"); i > 0 {
		trimmed = pod[:i]
	}
	switch {
	case labels["batch.kubernetes.io/job-name"] != "" || labels["job-name"] != "":
		w.job = labels["batch.kubernetes.io/job-name"]
		if w.job == "" {
			w.job = labels["job-name"]
		}
		if m := cronJobSuffix.FindStringSubmatch(w.job); m != nil && !created.IsZero() {
			minute, _ := strconv.ParseInt(m[2], 10, 64)
			if d := created.Sub(time.Unix(minute*60, 0)); d <= cronJobSkew && d >= -cronJobSkew {
				w.cronjob = m[1]
			}
		}
	case labels["statefulset.kubernetes.io/pod-name"] == pod:
		if m := ordinal.FindStringSubmatch(pod); m != nil {
			w.statefulset = m[1]
		}
	case labels["pod-template-hash"] != "":
		// the processor's rule: the ReplicaSet is <deployment>-<hash>
		hash := labels["pod-template-hash"]
		if strings.HasSuffix(trimmed, "-"+hash) {
			w.replicaset = trimmed
			w.deployment = strings.TrimSuffix(trimmed, "-"+hash)
		}
	case labels["controller-revision-hash"] != "":
		w.daemonset = trimmed
	}
	return w
}

// fields are the workload's row fields, by their names in the semantic
// conventions with dots as underscores, as Prometheus names them; the
// empty ones are left out.
func (w workload) fields() map[string]string {
	out := map[string]string{}
	for k, v := range map[string]string{
		"k8s_deployment_name": w.deployment, "k8s_replicaset_name": w.replicaset,
		"k8s_statefulset_name": w.statefulset, "k8s_daemonset_name": w.daemonset,
		"k8s_job_name": w.job, "k8s_cronjob_name": w.cronjob,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
