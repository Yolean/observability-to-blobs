package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func annotator(t *testing.T, monitors ...*monitoringv1.PodMonitor) (*podAnnotator, *bytes.Buffer) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := monitoringv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, pm := range monitors {
		b = b.WithObjects(pm)
	}
	var logs bytes.Buffer
	return &podAnnotator{
		monitors: b.Build(),
		decoder:  admission.NewDecoder(scheme),
		log:      slog.New(slog.NewJSONHandler(&logs, nil)),
	}, &logs
}

func request(t *testing.T, raw []byte, dryRun bool) admission.Request {
	t.Helper()
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "app",
		Object:    runtime.RawExtension{Raw: raw},
		DryRun:    &dryRun,
	}}
}

func TestHandleAnnotatesAndLogs(t *testing.T) {
	a, logs := annotator(t, podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics}]}`))
	raw, _ := json.Marshal(testPod(t, nil, mainAndWorker))
	resp := a.Handle(context.Background(), request(t, raw, false))
	if !resp.Allowed || len(resp.Patches) != 1 {
		t.Fatalf("%+v", resp)
	}
	if !strings.Contains(logs.String(), `"msg":"annotated"`) {
		t.Fatalf("logs %s", logs)
	}
}

// A server-side dry run gets the patch, so that it shows the result, but
// is not logged: no pod is created.
func TestHandleDryRunNotLogged(t *testing.T) {
	a, logs := annotator(t, podMonitor(t, "app", "pm", `{selector: {}, podMetricsEndpoints: [{port: metrics}, {port: nope}]}`))
	raw, _ := json.Marshal(testPod(t, nil, mainAndWorker))
	resp := a.Handle(context.Background(), request(t, raw, true))
	if !resp.Allowed || len(resp.Patches) != 1 || len(resp.Warnings) != 1 {
		t.Fatalf("%+v", resp)
	}
	if logs.Len() != 0 {
		t.Fatalf("logs %s", logs)
	}
}

// An object that does not decode is admitted, not denied.
func TestHandleUndecodableAllowed(t *testing.T) {
	a, logs := annotator(t)
	resp := a.Handle(context.Background(), request(t, []byte(`{"spec": 1}`), false))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("%+v", resp)
	}
	if !strings.Contains(logs.String(), "admitted unannotated") {
		t.Fatalf("logs %s", logs)
	}
}
