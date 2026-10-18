package main

import (
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReport(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	pm := podMonitor(t, "app", "pm", `{selector: {matchLabels: {app: x}}, podMetricsEndpoints: [{port: metrics}, {port: worker-metrics, scrapeTimeout: 5s}]}`)
	pm.CreationTimestamp = metav1.NewTime(t0)
	pm.Generation = 3
	pod := func(name string, created time.Time, ann map[string]string, labels map[string]string) corev1.Pod {
		p := testPod(t, ann, mainAndWorker)
		p.Name = name
		p.CreationTimestamp = metav1.NewTime(created)
		if labels != nil {
			p.Labels = labels
		}
		return *p
	}
	// the annotations the webhook writes for pm now
	now := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm}).Annotations
	// what an earlier generation of pm, without its second endpoint, wrote
	earlier := map[string]string{Marker: "app/pm", "io.opentelemetry.discovery.metrics.9201/enabled": "true"}
	done := pod("done", t0.Add(time.Hour), nil, nil)
	done.Status.Phase = corev1.PodSucceeded
	other := pod("elsewhere", t0.Add(time.Hour), nil, nil)
	other.Namespace = "other"
	r := NewReport(pm, []corev1.Pod{
		pod("b-annotated", t0.Add(time.Hour), now, nil),
		pod("c-stale", t0.Add(time.Hour), earlier, nil),
		pod("a-annotated", t0.Add(2*time.Hour), map[string]string{Marker: "app/other,app/pm"}, nil),
		pod("old", t0.Add(-time.Hour), nil, nil),
		pod("preexisting", t0.Add(time.Hour), map[string]string{"prometheus.io/scrape": "true"}, nil),
		pod("missed", t0.Add(time.Hour), nil, nil),
		pod("unselected", t0.Add(time.Hour), nil, map[string]string{"app": "y"}),
		done, other,
	})
	if r.PodMonitor != "app/pm" || r.Generation != 3 || r.Condition != PartiallyTranslated {
		t.Fatalf("%+v", r)
	}
	if len(r.Untranslated) != 1 || r.Untranslated[0] != "podMetricsEndpoints[1].scrapeTimeout" {
		t.Fatalf("untranslated %v", r.Untranslated)
	}
	if r.AnnotatedCount != 2 || r.Annotated[0].Pod != "app/a-annotated" || !r.Annotated[1].Created.Equal(t0.Add(time.Hour)) {
		t.Fatalf("annotated %+v", r.Annotated)
	}
	if r.StaleCount != 1 || r.Stale[0].Pod != "app/c-stale" {
		t.Fatalf("stale %+v", r.Stale)
	}
	reasons := map[string]string{}
	for _, s := range r.Skipped {
		reasons[s.Pod] = s.Reason
	}
	if r.SkippedCount != 2 || len(reasons) != 2 {
		t.Fatalf("skipped %+v", r.Skipped)
	}
	// annotated in its own source: not skipped, migrated
	if r.PreannotatedCount != 1 || r.Preannotated[0].Pod != "app/preexisting" {
		t.Fatalf("preannotated %+v", r.Preannotated)
	}
	if !strings.HasPrefix(reasons["app/old"], "created before the PodMonitor") ||
		!strings.HasPrefix(reasons["app/missed"], "not annotated at admission") {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestReportConditionAndSample(t *testing.T) {
	pm := podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics}]}`)
	var pods []corev1.Pod
	ann := Translate("app/p", testPod(t, nil, mainAndWorker), []*monitoringv1.PodMonitor{pm}).Annotations
	for i := 0; i < reportSample+5; i++ {
		p := testPod(t, ann, mainAndWorker)
		p.Name = "p" + string(rune('a'+i))
		pods = append(pods, *p)
	}
	r := NewReport(pm, pods)
	if r.Condition != Translated || r.AnnotatedCount != reportSample+5 || len(r.Annotated) != reportSample {
		t.Fatalf("%+v", r)
	}
	empty := podMonitor(t, "app", "empty", `{selector: {}, podMetricsEndpoints: []}`)
	if r := NewReport(empty, nil); r.Condition != NotTranslated {
		t.Fatalf("%+v", r)
	}
}
