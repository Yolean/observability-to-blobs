package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	otelPrefix = "io.opentelemetry.discovery."
	promPrefix = "prometheus.io/"
	// Marker lists the PodMonitors (namespace/name) whose endpoints the
	// webhook translated into the pod's annotations. It carries no scrape
	// meaning; the report reads it to tell the pods annotated at admission
	// from the ones that were not.
	Marker = "observability-to-blobs/podmonitors"
)

// Selects is whether pm selects a pod with these labels in namespace, the
// way prometheus-operator does for a Prometheus that selects every
// PodMonitor: spec.namespaceSelector, then spec.selector.
func Selects(pm *monitoringv1.PodMonitor, namespace string, podLabels map[string]string) (bool, error) {
	ns := pm.Spec.NamespaceSelector
	switch {
	case ns.Any:
	case len(ns.MatchNames) > 0:
		found := false
		for _, n := range ns.MatchNames {
			found = found || n == namespace
		}
		if !found {
			return false, nil
		}
	default:
		if namespace != pm.Namespace {
			return false, nil
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(&pm.Spec.Selector)
	if err != nil {
		return false, fmt.Errorf("spec.selector: %w", err)
	}
	return sel.Matches(labels.Set(podLabels)), nil
}

// HasScrapeAnnotations is whether the pod already carries annotations of
// either convention the agent reads.
func HasScrapeAnnotations(ann map[string]string) bool {
	for k := range ann {
		if strings.HasPrefix(k, promPrefix) || strings.HasPrefix(k, otelPrefix) {
			return true
		}
	}
	return false
}

// Port is a container port an endpoint resolved to.
type Port struct {
	Number    int32
	Container string // "" when no container declares the number
}

// declaredPorts are the pod's TCP container ports, containers in spec
// order, restartable init containers (sidecars) after them.
func declaredPorts(spec *corev1.PodSpec) []Port {
	var out []Port
	add := func(c corev1.Container) {
		for _, p := range c.Ports {
			if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
				out = append(out, Port{Number: p.ContainerPort, Container: c.Name})
			}
		}
	}
	for _, c := range spec.Containers {
		add(c)
	}
	for _, c := range spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			add(c)
		}
	}
	return out
}

func portsNamed(spec *corev1.PodSpec, name string) []Port {
	var out []Port
	add := func(c corev1.Container) {
		for _, p := range c.Ports {
			if p.Name == name && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
				out = append(out, Port{Number: p.ContainerPort, Container: c.Name})
			}
		}
	}
	for _, c := range spec.Containers {
		add(c)
	}
	for _, c := range spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			add(c)
		}
	}
	return out
}

func portNumbered(spec *corev1.PodSpec, n int32) Port {
	for _, p := range declaredPorts(spec) {
		if p.Number == n {
			return p
		}
	}
	return Port{Number: n}
}

// resolve is the container ports an endpoint scrapes in this pod, as
// prometheus-operator's relabeling would keep them: by port name every
// container port of that name, by number that number, and with neither
// every declared port. A message says why it is none, or that a number
// has no container.
func resolve(ep *monitoringv1.PodMetricsEndpoint, spec *corev1.PodSpec) ([]Port, string) {
	switch {
	case ep.Port != nil && *ep.Port != "":
		ports := portsNamed(spec, *ep.Port)
		if len(ports) == 0 {
			return nil, fmt.Sprintf("no container declares a port named %q", *ep.Port)
		}
		return ports, ""
	case ep.PortNumber != nil:
		return number(spec, *ep.PortNumber)
	case ep.TargetPort != nil:
		if ep.TargetPort.Type == intstr.Int {
			return number(spec, ep.TargetPort.IntVal)
		}
		ports := portsNamed(spec, ep.TargetPort.StrVal)
		if len(ports) == 0 {
			return nil, fmt.Sprintf("no container declares a port named %q (targetPort)", ep.TargetPort.StrVal)
		}
		return ports, ""
	}
	ports := declaredPorts(spec)
	if len(ports) == 0 {
		return nil, "the endpoint names no port and the pod declares none"
	}
	return ports, ""
}

func number(spec *corev1.PodSpec, n int32) ([]Port, string) {
	p := portNumbered(spec, n)
	if p.Container == "" {
		return []Port{p}, fmt.Sprintf("no container declares port %d: its samples will have no container; declare the port", n)
	}
	return []Port{p}, ""
}

// Fields the agent does what the value says anyway, so they need no
// translation and no warning.
var agentEquivalent = map[string]func(v any) bool{
	"scheme":          func(v any) bool { return strings.EqualFold(fmt.Sprint(v), "http") },
	"honorLabels":     func(v any) bool { return v == false },
	"filterRunning":   func(v any) bool { return v == true },
	"followRedirects": func(v any) bool { return v == true },
	// no effect on a plain http scrape
	"enableHttp2": func(any) bool { return true },
}

var (
	// endpoint fields the translation carries
	translatedEndpointFields = map[string]bool{"port": true, "portNumber": true, "targetPort": true, "path": true, "interval": true}
	// spec fields the selection carries; selectorMechanism only chooses
	// how the operator implements the same selection. jobLabel,
	// podTargetLabels and attachMetadata label a target with pod metadata:
	// the agent's rows carry the pod's identity (namespace, pod, pod_uid,
	// container) instead, and its podindex the pod's labels, so a consumer
	// joins for them. That is a gap only for a consumer that expects the
	// operator's job label or label copies on each sample; they do not
	// count as untranslated.
	translatedSpecFields = map[string]bool{"selector": true, "namespaceSelector": true, "podMetricsEndpoints": true, "selectorMechanism": true,
		"jobLabel": true, "podTargetLabels": true, "attachMetadata": true}
)

// untranslated lists the fields set in v (a spec or an endpoint) that
// the translation cannot carry, by their JSON names, sorted.
func untranslated(v any, translated map[string]bool) []string {
	raw, err := json.Marshal(v)
	if err != nil {
		return []string{fmt.Sprintf("(unreadable: %v)", err)}
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return []string{fmt.Sprintf("(unreadable: %v)", err)}
	}
	var out []string
	for k, v := range fields {
		if translated[k] {
			continue
		}
		if eq, ok := agentEquivalent[k]; ok && eq(v) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Untranslated is the fields of pm that the annotations cannot carry:
// the spec's own, and each endpoint's by index.
func Untranslated(pm *monitoringv1.PodMonitor) (spec []string, endpoints [][]string) {
	s := pm.Spec
	s.PodMetricsEndpoints = nil
	spec = untranslated(s, translatedSpecFields)
	for i := range pm.Spec.PodMetricsEndpoints {
		endpoints = append(endpoints, untranslated(pm.Spec.PodMetricsEndpoints[i], translatedEndpointFields))
	}
	return spec, endpoints
}

type otelConfig struct {
	MetricsPath        string `json:"metrics_path,omitempty"`
	CollectionInterval string `json:"collection_interval,omitempty"`
}

// Contribution is what one PodMonitor adds to a pod.
type Contribution struct {
	PodMonitor string // namespace/name
	Ports      []Port
	// Reason is why the PodMonitor adds nothing, when it adds nothing
	Reason string
}

// Result is the translation of a pod against the PodMonitors that select it.
type Result struct {
	// Annotations to add; empty when the pod is left alone
	Annotations map[string]string
	// Contributions, one per selecting PodMonitor, in the order applied
	Contributions []Contribution
	// Preexisting: the pod already has scrape annotations, nothing is added
	Preexisting bool
	Warnings    []string
}

func key(pm *monitoringv1.PodMonitor) string { return pm.Namespace + "/" + pm.Name }

// Translate adds, for each endpoint of each PodMonitor that selects the
// pod, the port-scoped OpenTelemetry annotations of each port it
// resolves to, by number: enabled, and config with metrics_path and
// collection_interval when the endpoint sets them. Never the pod-level
// keys. selecting must hold only PodMonitors that select the pod; podName
// names it in warnings. A port that two endpoints resolve to keeps the
// first, PodMonitors in namespace/name order and endpoints in theirs.
func Translate(podName string, pod *corev1.Pod, selecting []*monitoringv1.PodMonitor) Result {
	monitors := append([]*monitoringv1.PodMonitor(nil), selecting...)
	sort.Slice(monitors, func(i, j int) bool { return key(monitors[i]) < key(monitors[j]) })
	res := Result{}
	if len(monitors) == 0 {
		return res
	}
	names := make([]string, len(monitors))
	for i, pm := range monitors {
		names[i] = key(pm)
	}
	if HasScrapeAnnotations(pod.Annotations) {
		res.Preexisting = true
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"pod %s already has prometheus.io/* or io.opentelemetry.discovery.* annotations: not applying PodMonitor %s",
			podName, strings.Join(names, ", ")))
		for _, n := range names {
			res.Contributions = append(res.Contributions, Contribution{PodMonitor: n, Reason: "pre-existing scrape annotations"})
		}
		return res
	}

	type taken struct {
		by  string
		cfg otelConfig
	}
	byPort := map[int32]taken{}
	ann := map[string]string{}
	var applied []string
	for _, pm := range monitors {
		c := Contribution{PodMonitor: key(pm)}
		var reasons []string
		for i := range pm.Spec.PodMetricsEndpoints {
			ep := &pm.Spec.PodMetricsEndpoints[i]
			where := fmt.Sprintf("PodMonitor %s endpoint %d", key(pm), i)
			ports, msg := resolve(ep, &pod.Spec)
			if msg != "" {
				res.Warnings = append(res.Warnings, fmt.Sprintf("pod %s, %s: %s", podName, where, msg))
				if len(ports) == 0 {
					reasons = append(reasons, msg)
				}
			}
			cfg := otelConfig{MetricsPath: ep.Path}
			if ep.Interval != "" {
				d, err := model.ParseDuration(string(ep.Interval))
				if err != nil || d <= 0 {
					res.Warnings = append(res.Warnings, fmt.Sprintf("pod %s, %s: interval %q is not a positive duration, not translated", podName, where, ep.Interval))
				} else {
					cfg.CollectionInterval = time.Duration(d).String()
				}
			}
			for _, p := range ports {
				if t, ok := byPort[p.Number]; ok {
					if t.cfg != cfg {
						res.Warnings = append(res.Warnings, fmt.Sprintf("pod %s, %s: port %d is already scraped as %s says", podName, where, p.Number, t.by))
					}
					reasons = append(reasons, fmt.Sprintf("port %d taken by %s", p.Number, t.by))
					continue
				}
				byPort[p.Number] = taken{by: where, cfg: cfg}
				prefix := "io.opentelemetry.discovery.metrics." + strconv.Itoa(int(p.Number))
				ann[prefix+"/enabled"] = "true"
				if cfg != (otelConfig{}) {
					y, _ := yaml.Marshal(cfg)
					ann[prefix+"/config"] = string(y)
				}
				c.Ports = append(c.Ports, p)
			}
		}
		if len(c.Ports) == 0 {
			c.Reason = "no endpoint resolves to a port of the pod"
			if len(pm.Spec.PodMetricsEndpoints) == 0 {
				c.Reason = "the PodMonitor has no endpoints"
			} else if len(reasons) > 0 {
				c.Reason += ": " + strings.Join(reasons, "; ")
			}
		} else {
			applied = append(applied, key(pm))
		}
		res.Contributions = append(res.Contributions, c)
	}
	if len(applied) > 0 {
		ann[Marker] = strings.Join(applied, ",")
		res.Annotations = ann
	}
	return res
}

// MarkedBy is the PodMonitors the pod's marker lists.
func MarkedBy(ann map[string]string) []string {
	v := ann[Marker]
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}
