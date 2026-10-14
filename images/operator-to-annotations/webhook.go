package main

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"gomodules.xyz/jsonpatch/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// podAnnotator is the mutating webhook for Pod CREATE: it adds the
// annotations of the PodMonitors that select the pod. It never denies:
// anything it cannot do is a warning, and the pod is admitted as it came.
type podAnnotator struct {
	monitors client.Reader
	decoder  admission.Decoder
	log      *slog.Logger
}

func (a *podAnnotator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := a.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	name := pod.Name
	if name == "" {
		name = pod.GenerateName + "*"
	}
	podName := req.Namespace + "/" + name

	var list monitoringv1.PodMonitorList
	if err := a.monitors.List(ctx, &list); err != nil {
		a.log.Error("listing PodMonitors, pod admitted unannotated", "pod", podName, "err", err)
		return admission.Allowed("")
	}
	var selecting []*monitoringv1.PodMonitor
	for i := range list.Items {
		pm := &list.Items[i]
		ok, err := Selects(pm, req.Namespace, pod.Labels)
		if err != nil {
			a.log.Warn("PodMonitor selector", "podMonitor", key(pm), "err", err)
			continue
		}
		if ok {
			selecting = append(selecting, pm)
		}
	}
	res := Translate(podName, pod, selecting)
	for _, w := range res.Warnings {
		a.log.Warn(w, "pod", podName)
	}
	if len(res.Annotations) == 0 {
		return admission.Allowed("").WithWarnings(res.Warnings...)
	}
	a.log.Info("annotated", "pod", podName, "podMonitors", res.Annotations[Marker], "annotations", len(res.Annotations)-1)
	resp := admission.Allowed("")
	resp.Patches = annotationPatch(pod.Annotations, res.Annotations)
	return resp.WithWarnings(res.Warnings...)
}

// annotationPatch adds annotations to metadata.annotations, which is
// absent when existing is nil.
func annotationPatch(existing, add map[string]string) []jsonpatch.Operation {
	if existing == nil {
		m := map[string]any{}
		for k, v := range add {
			m[k] = v
		}
		return []jsonpatch.Operation{{Operation: "add", Path: "/metadata/annotations", Value: m}}
	}
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ops := make([]jsonpatch.Operation, 0, len(keys))
	for _, k := range keys {
		ops = append(ops, jsonpatch.Operation{Operation: "add", Path: "/metadata/annotations/" + escapePointer(k), Value: add[k]})
	}
	return ops
}

// escapePointer escapes a JSON Pointer reference token (RFC 6901).
func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
