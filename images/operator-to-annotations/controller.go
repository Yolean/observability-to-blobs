package main

import (
	"context"
	"log/slog"
	"reflect"
	"sync"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reporter keeps a Report per PodMonitor and logs it when it changes: at
// level WARN while the PodMonitor is not translated in full or has
// selected pods without its annotations, INFO otherwise.
type reporter struct {
	client client.Reader
	log    *slog.Logger
	mu     sync.Mutex
	last   map[types.NamespacedName]Report
}

func newReporter(c client.Reader, log *slog.Logger) *reporter {
	return &reporter{client: c, log: log, last: map[types.NamespacedName]Report{}}
}

func (r *reporter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pm monitoringv1.PodMonitor
	if err := r.client.Get(ctx, req.NamespacedName, &pm); err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			if _, known := r.last[req.NamespacedName]; known {
				delete(r.last, req.NamespacedName)
				r.log.Info("PodMonitor deleted", "podMonitor", req.String())
			}
			r.mu.Unlock()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	var pods []corev1.Pod
	for _, ns := range podNamespaces(&pm) {
		var list corev1.PodList
		if err := r.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return ctrl.Result{}, err
		}
		pods = append(pods, list.Items...)
	}
	rep := NewReport(&pm, pods)
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.last[req.NamespacedName]; ok && reflect.DeepEqual(prev, rep) {
		return ctrl.Result{}, nil
	}
	r.last[req.NamespacedName] = rep
	level := slog.LevelInfo
	if rep.Condition != Translated || rep.SkippedCount > 0 {
		level = slog.LevelWarn
	}
	r.log.Log(ctx, level, "report", "report", rep)
	return ctrl.Result{}, nil
}

// podNamespaces is where pm's pods can be: "" for every namespace.
func podNamespaces(pm *monitoringv1.PodMonitor) []string {
	switch {
	case pm.Spec.NamespaceSelector.Any:
		return []string{""}
	case len(pm.Spec.NamespaceSelector.MatchNames) > 0:
		return pm.Spec.NamespaceSelector.MatchNames
	}
	return []string{pm.Namespace}
}

// monitorsOf maps a pod to the PodMonitors that select it, so that a pod's
// creation, change or deletion updates their reports.
func monitorsOf(c client.Reader, log *slog.Logger) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list monitoringv1.PodMonitorList
		if err := c.List(ctx, &list); err != nil {
			log.Error("listing PodMonitors", "err", err)
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			pm := &list.Items[i]
			if ok, _ := Selects(pm, o.GetNamespace(), o.GetLabels()); ok {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pm.Namespace, Name: pm.Name}})
			}
		}
		return out
	}
}

// serviceMonitors logs each ServiceMonitor once per generation: they are
// not translated, so their targets are not scraped.
type serviceMonitors struct {
	client client.Reader
	log    *slog.Logger
	mu     sync.Mutex
	seen   map[types.NamespacedName]int64
}

func (s *serviceMonitors) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sm monitoringv1.ServiceMonitor
	if err := s.client.Get(ctx, req.NamespacedName, &sm); err != nil {
		if apierrors.IsNotFound(err) {
			s.mu.Lock()
			delete(s.seen, req.NamespacedName)
			s.mu.Unlock()
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[req.NamespacedName] == sm.Generation {
		return ctrl.Result{}, nil
	}
	s.seen[req.NamespacedName] = sm.Generation
	s.log.Warn("ServiceMonitor is not supported, its targets are not scraped", "serviceMonitor", req.String())
	return ctrl.Result{}, nil
}
