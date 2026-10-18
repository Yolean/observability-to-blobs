package main

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
)

// Condition says how much of a PodMonitor the annotations carry.
type Condition string

const (
	// every endpoint, and the spec, is carried in full
	Translated Condition = "Translated"
	// some fields are not carried (Report.Untranslated)
	PartiallyTranslated Condition = "PartiallyTranslated"
	// no endpoint is carried
	NotTranslated Condition = "NotTranslated"
)

// Pods in a report are listed up to this many per list; the counts are
// always complete.
const reportSample = 20

// PodRef is a pod in a report: namespace/name and its creation, which for
// an annotated pod is when it was admitted.
type PodRef struct {
	Pod     string    `json:"pod"`
	Created time.Time `json:"created"`
}

// SkippedPod is a selected pod that the webhook did not annotate for the
// PodMonitor, and why.
type SkippedPod struct {
	PodRef
	Reason string `json:"reason"`
}

// Report is where a PodMonitor is in use: what of it the annotations carry
// and which of the pods it selects have them. The controller keeps one per
// PodMonitor and logs it when it changes; the PodMonitor's status is not
// written (README.md, "Reporting").
type Report struct {
	PodMonitor string    `json:"podMonitor"`
	Generation int64     `json:"generation"`
	Condition  Condition `json:"condition"`
	// Untranslated is the fields the annotations cannot carry, "spec.<field>"
	// and "podMetricsEndpoints[<i>].<field>"
	Untranslated []string `json:"untranslated,omitempty"`
	// Annotated is the selected pods the webhook annotated for this
	// PodMonitor; AnnotatedCount counts all of them, Annotated lists the
	// first reportSample by name
	AnnotatedCount int      `json:"annotatedCount"`
	Annotated      []PodRef `json:"annotated,omitempty"`
	// Stale is the pods annotated for this PodMonitor alone whose scrape
	// annotations differ from what it translates to now: an earlier
	// generation of it, or an earlier version of the webhook, wrote them.
	// They scrape as those say until they are recreated.
	StaleCount int      `json:"staleCount"`
	Stale      []PodRef `json:"stale,omitempty"`
	// Preannotated is the selected pods with scrape annotations of their
	// own, which the webhook leaves alone: where the migration to
	// annotations in the workload's source is done
	PreannotatedCount int      `json:"preannotatedCount"`
	Preannotated      []PodRef `json:"preannotated,omitempty"`
	// Skipped is the selected pods it did not annotate for another reason,
	// with the reason
	SkippedCount int          `json:"skippedCount"`
	Skipped      []SkippedPod `json:"skipped,omitempty"`
}

// Reasons for a skipped pod, besides the translation's own (Contribution.Reason)
const (
	reasonBeforePodMonitor = "created before the PodMonitor; recreate the pod"
	reasonNotAdmitted      = "not annotated at admission: created before the webhook was installed, or while it was down; recreate the pod"
)

// NewReport computes pm's report over pods, the cluster's pods in the
// namespaces pm selects from (others are ignored). A pod's selection is
// as at admission: its labels now.
func NewReport(pm *monitoringv1.PodMonitor, pods []corev1.Pod) Report {
	r := Report{PodMonitor: key(pm), Generation: pm.Generation}
	spec, endpoints := Untranslated(pm)
	for _, f := range spec {
		r.Untranslated = append(r.Untranslated, "spec."+f)
	}
	for i, fs := range endpoints {
		for _, f := range fs {
			r.Untranslated = append(r.Untranslated, "podMetricsEndpoints["+strconv.Itoa(i)+"]."+f)
		}
	}
	switch {
	case len(pm.Spec.PodMetricsEndpoints) == 0:
		r.Condition = NotTranslated
	case len(r.Untranslated) > 0:
		r.Condition = PartiallyTranslated
	default:
		r.Condition = Translated
	}

	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if ok, err := Selects(pm, pod.Namespace, pod.Labels); err != nil || !ok {
			continue
		}
		ref := PodRef{Pod: pod.Namespace + "/" + pod.Name, Created: pod.CreationTimestamp.UTC()}
		marked := MarkedBy(pod.Annotations)
		if slices.Equal(marked, []string{r.PodMonitor}) && stale(pm, pod) {
			r.StaleCount++
			r.Stale = append(r.Stale, ref)
			continue
		}
		if slices.Contains(marked, r.PodMonitor) {
			r.AnnotatedCount++
			r.Annotated = append(r.Annotated, ref)
			continue
		}
		if len(marked) == 0 && HasScrapeAnnotations(pod.Annotations) {
			r.PreannotatedCount++
			r.Preannotated = append(r.Preannotated, ref)
			continue
		}
		r.SkippedCount++
		r.Skipped = append(r.Skipped, SkippedPod{PodRef: ref, Reason: skipReason(pm, pod)})
	}
	sort.Slice(r.Annotated, func(i, j int) bool { return r.Annotated[i].Pod < r.Annotated[j].Pod })
	sort.Slice(r.Stale, func(i, j int) bool { return r.Stale[i].Pod < r.Stale[j].Pod })
	sort.Slice(r.Preannotated, func(i, j int) bool { return r.Preannotated[i].Pod < r.Preannotated[j].Pod })
	sort.Slice(r.Skipped, func(i, j int) bool { return r.Skipped[i].Pod < r.Skipped[j].Pod })
	if len(r.Annotated) > reportSample {
		r.Annotated = r.Annotated[:reportSample]
	}
	if len(r.Stale) > reportSample {
		r.Stale = r.Stale[:reportSample]
	}
	if len(r.Preannotated) > reportSample {
		r.Preannotated = r.Preannotated[:reportSample]
	}
	if len(r.Skipped) > reportSample {
		r.Skipped = r.Skipped[:reportSample]
	}
	return r
}

// stale is whether a pod annotated for pm alone has other scrape
// annotations than pm translates to now. A pod that several PodMonitors
// annotated is not checked: which ports each took depends on all of them.
func stale(pm *monitoringv1.PodMonitor, pod *corev1.Pod) bool {
	want := Translate(pod.Namespace+"/"+pod.Name, &corev1.Pod{Spec: pod.Spec}, []*monitoringv1.PodMonitor{pm}).Annotations
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, otelPrefix) && want[k] != v {
			return true
		}
	}
	for k, v := range want {
		if k != Marker && pod.Annotations[k] != v {
			return true
		}
	}
	return false
}

// skipReason is why a pod that pm selects carries no annotations of pm.
func skipReason(pm *monitoringv1.PodMonitor, pod *corev1.Pod) string {
	if pod.CreationTimestamp.Before(&pm.CreationTimestamp) {
		return reasonBeforePodMonitor
	}
	marked := MarkedBy(pod.Annotations)
	// what this PodMonitor alone would add: a reason of its own (no port,
	// or a port another PodMonitor took) shows in the translation
	res := Translate(pod.Namespace+"/"+pod.Name, &corev1.Pod{Spec: pod.Spec}, []*monitoringv1.PodMonitor{pm})
	if c := res.Contributions; len(c) == 1 && c[0].Reason != "" {
		return c[0].Reason
	}
	if len(marked) > 0 {
		return "another PodMonitor's endpoints took its ports"
	}
	return reasonNotAdmitted
}
