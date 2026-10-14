// operator-to-annotations translates prometheus-operator PodMonitors into
// the port-scoped OpenTelemetry discovery annotations that the node agent
// reads, when pods are admitted (operator-to-annotations/README.md).
//
// Env:
//
//	WATCH_NAMESPACES   comma-separated namespaces whose PodMonitors and
//	                   ServiceMonitors count; empty for all
//	CERT_DIR           the webhook's serving tls.crt and tls.key (/tls)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fatal(log, "scheme", err)
	}
	if err := monitoringv1.AddToScheme(scheme); err != nil {
		fatal(log, "scheme", err)
	}

	cacheOpts := cache.Options{DefaultTransform: cache.TransformStripManagedFields()}
	if v := strings.TrimSpace(os.Getenv("WATCH_NAMESPACES")); v != "" {
		namespaces := map[string]cache.Config{}
		for _, ns := range strings.Split(v, ",") {
			namespaces[strings.TrimSpace(ns)] = cache.Config{}
		}
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&monitoringv1.PodMonitor{}:     {Namespaces: namespaces},
			&monitoringv1.ServiceMonitor{}: {Namespaces: namespaces},
		}
	}
	certDir := os.Getenv("CERT_DIR")
	if certDir == "" {
		certDir = "/tls"
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: ":8080"},
		HealthProbeBindAddress: ":8081",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 9443, CertDir: certDir}),
	})
	if err != nil {
		fatal(log, "manager", err)
	}

	mgr.GetWebhookServer().Register("/mutate-pod", &webhook.Admission{Handler: &podAnnotator{
		monitors: mgr.GetCache(),
		decoder:  admission.NewDecoder(scheme),
		log:      log.With("component", "webhook"),
	}})
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1.PodMonitor{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(monitorsOf(mgr.GetCache(), log))).
		Complete(newReporter(mgr.GetCache(), log.With("component", "report"))); err != nil {
		fatal(log, "PodMonitor controller", err)
	}
	pmKind := schema.GroupKind{Group: monitoringv1.SchemeGroupVersion.Group, Kind: monitoringv1.PodMonitorsKind}
	if _, err := mgr.GetRESTMapper().RESTMapping(pmKind, monitoringv1.Version); err != nil {
		fatal(log, "the PodMonitor CRD (operator-to-annotations/crds/podmonitors)", err)
	}
	// The ServiceMonitor CRD may be left out (operator-to-annotations/crds
	// has a base per CRD); then there is nothing to warn about.
	smKind := schema.GroupKind{Group: monitoringv1.SchemeGroupVersion.Group, Kind: monitoringv1.ServiceMonitorsKind}
	if _, err := mgr.GetRESTMapper().RESTMapping(smKind, monitoringv1.Version); meta.IsNoMatchError(err) {
		log.Info("no ServiceMonitor CRD, ServiceMonitors are not watched")
	} else if err := ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1.ServiceMonitor{}).
		Complete(&serviceMonitors{client: mgr.GetCache(), log: log.With("component", "servicemonitor"), seen: map[types.NamespacedName]int64{}}); err != nil {
		fatal(log, "ServiceMonitor controller", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		fatal(log, "healthz", err)
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		fatal(log, "readyz", err)
	}
	// Ready only once the PodMonitors are in the cache: a pod admitted
	// before would be admitted unannotated.
	var synced atomic.Bool
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if mgr.GetCache().WaitForCacheSync(ctx) {
			synced.Store(true)
		}
		<-ctx.Done()
		return nil
	})); err != nil {
		fatal(log, "cache sync", err)
	}
	if err := mgr.AddReadyzCheck("cache", func(*http.Request) error {
		if !synced.Load() {
			return errors.New("cache not synced")
		}
		return nil
	}); err != nil {
		fatal(log, "readyz", err)
	}
	log.Info("starting", "watchNamespaces", os.Getenv("WATCH_NAMESPACES"))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fatal(log, "manager", err)
	}
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}
