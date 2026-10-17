package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/relabel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Scrape targets are discovered from the container runtime alone. The kubelet
// hands every pod annotation and label to the CRI at RunPodSandbox, and
// containerd keeps them for the sandbox's lifetime, so no Kubernetes API
// access is needed. Container-level data (declared ports, restart count)
// arrives as io.kubernetes.container.* annotations on each container.
//
// Two annotation conventions are honored, prometheus.io/* and the
// OpenTelemetry collector's io.opentelemetry.discovery.metrics[.<port>]/*;
// their specification is the root README's "Scrape annotations".
const (
	annScrape = "prometheus.io/scrape"
	annPort   = "prometheus.io/port"
	annPath   = "prometheus.io/path"
	annScheme = "prometheus.io/scheme"

	otelPrefix = "io.opentelemetry.discovery.metrics"

	defaultPath = "/metrics"

	criContainerPorts = "io.kubernetes.container.ports"
	// labels the kubelet puts on every container it creates
	criPodUIDLabel        = "io.kubernetes.pod.uid"
	criContainerNameLabel = "io.kubernetes.container.name"
)

var otelKeyRe = regexp.MustCompile(`^io\.opentelemetry\.discovery\.metrics(?:\.([^/]+))?/(enabled|scraper|config)$`)

type target struct {
	key       string // stable identity across discovery rounds
	namespace string
	pod       string
	podUID    string
	container string
	restart   *int32 // the named container's restart count; nil when no running container has the name
	scheme    string
	host      string
	port      int
	path      string
	interval  time.Duration // 0 means the scraper's default
	relabel   []*relabel.Config
	// relabelErr is why the target's metric_relabel_configs are not
	// applied; its samples are then shipped as scraped
	relabelErr string
}

func (t target) url() string {
	return fmt.Sprintf("%s://%s:%d%s", t.scheme, t.host, t.port, t.path)
}

func (t target) instance() string {
	return fmt.Sprintf("%s:%d", t.host, t.port)
}

type containerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

// declaredPort is a containerPort together with the container declaring it.
type declaredPort struct {
	container string
	port      containerPort
}

// otelHint is one io.opentelemetry.discovery.metrics[.<port>]/* group.
type otelHint struct {
	enabled *bool
	scraper string
	config  otelConfig
}

type otelConfig struct {
	Endpoint           string `yaml:"endpoint"`
	MetricsPath        string `yaml:"metrics_path"`
	CollectionInterval string `yaml:"collection_interval"`
	// decoded per target, so that a malformed list does not take the
	// other fields with it
	MetricRelabelConfigs yaml.Node `yaml:"metric_relabel_configs"`
}

type criDiscoverer struct {
	client  runtime.RuntimeServiceClient
	conn    *grpc.ClientConn
	sbCache map[string]sandboxFacts // by sandbox id
	timeout time.Duration
	nodeIP  string // used for hostNetwork sandboxes, which report no IP
}

func newCRIDiscoverer(socket string, timeout time.Duration, nodeIP string) (*criDiscoverer, error) {
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("cri dial %s: %w", socket, err)
	}
	return &criDiscoverer{
		client:  runtime.NewRuntimeServiceClient(conn),
		conn:    conn,
		sbCache: map[string]sandboxFacts{},
		timeout: timeout,
		nodeIP:  nodeIP,
	}, nil
}

// ownRestart is the restart count of this agent's own container, for the
// static targets, which carry the agent's identity; nil when it cannot be
// found, and the rows then lack restart.
func (d *criDiscoverer) ownRestart(ctx context.Context, podUID, container string) *int32 {
	if podUID == "" || container == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	resp, err := d.client.ListContainers(ctx, &runtime.ListContainersRequest{Filter: &runtime.ContainerFilter{
		LabelSelector: map[string]string{criPodUIDLabel: podUID, criContainerNameLabel: container},
	}})
	if err != nil {
		log.Printf("[cri_scrape] own restart count: %v", err)
		return nil
	}
	return runningRestart(resp.Containers)
}

// runningRestart is the restart count of the running container among a
// pod's containers of one name; earlier runs are listed until removed.
func runningRestart(cs []*runtime.Container) *int32 {
	for _, c := range cs {
		if c.State == runtime.ContainerState_CONTAINER_RUNNING {
			r := containerRestart(c)
			return &r
		}
	}
	return nil
}

func (d *criDiscoverer) close() {
	d.conn.Close()
}

// inventory is one discovery round's raw view, consumed by the scrape
// target diff and by the podindex tracker.
type inventory struct {
	sandboxes  []*runtime.PodSandbox
	containers map[string][]*runtime.Container // by sandbox id, all states
}

// discover lists ready sandboxes and their containers (all states) and
// returns the scrape targets implied by their annotations plus the raw
// inventory for the podindex.
func (d *criDiscoverer) discover(ctx context.Context) (map[string]target, *inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	sandboxes, err := d.client.ListPodSandbox(ctx, &runtime.ListPodSandboxRequest{
		Filter: &runtime.PodSandboxFilter{
			State: &runtime.PodSandboxStateValue{State: runtime.PodSandboxState_SANDBOX_READY},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ListPodSandbox: %w", err)
	}
	containers, err := d.client.ListContainers(ctx, &runtime.ListContainersRequest{})
	if err != nil {
		return nil, nil, fmt.Errorf("ListContainers: %w", err)
	}

	bySandbox := map[string][]*runtime.Container{}
	running := map[string][]*runtime.Container{}
	for _, c := range containers.Containers {
		bySandbox[c.PodSandboxId] = append(bySandbox[c.PodSandboxId], c)
		if c.State == runtime.ContainerState_CONTAINER_RUNNING {
			running[c.PodSandboxId] = append(running[c.PodSandboxId], c)
		}
	}
	inv := &inventory{sandboxes: sandboxes.Items, containers: bySandbox}

	live := map[string]bool{}
	targets := map[string]target{}
	for _, sb := range sandboxes.Items {
		live[sb.Id] = true
		if sb.Metadata == nil {
			continue
		}
		expanded := expandTargets(sb.Annotations, declaredPorts(running[sb.Id]))
		if len(expanded) == 0 {
			continue
		}
		facts, err := d.sandboxStatus(ctx, sb.Id)
		if err != nil {
			log.Printf("[cri_scrape] %s/%s: %v", sb.Metadata.Namespace, sb.Metadata.Name, err)
			continue
		}
		ip := facts.podIP
		if ip == "" {
			// hostNetwork sandboxes report no IP of their own
			ip = d.nodeIP
		}
		if ip == "" {
			log.Printf("[cri_scrape] %s/%s: no pod IP and no node_ip configured, skipping", sb.Metadata.Namespace, sb.Metadata.Name)
			continue
		}
		for _, t := range podTargets(sb, running[sb.Id], expanded, ip) {
			targets[t.key] = t
		}
	}
	for id := range d.sbCache {
		if !live[id] {
			delete(d.sbCache, id)
		}
	}
	return targets, inv, nil
}

// podTargets completes a ready sandbox's expanded targets with the pod's
// identity, the address and the restart count of the container each names.
// The restart count is part of the key: discovery keeps a known key's first
// target, so a restarted container's scraper must be replaced, not kept.
func podTargets(sb *runtime.PodSandbox, running []*runtime.Container, expanded []target, ip string) []target {
	restarts := map[string]int32{}
	for _, c := range running {
		if c.Metadata != nil {
			restarts[c.Metadata.Name] = containerRestart(c)
		}
	}
	out := make([]target, 0, len(expanded))
	for _, t := range expanded {
		t.namespace = sb.Metadata.Namespace
		t.pod = sb.Metadata.Name
		t.podUID = sb.Metadata.Uid
		t.host = ip
		restart := ""
		if r, ok := restarts[t.container]; ok && t.container != "" {
			t.restart = &r
			restart = strconv.Itoa(int(r))
		}
		t.key = sb.Id + "/" + t.container + "/" + restart + "/" + strconv.Itoa(t.port)
		out = append(out, t)
	}
	return out
}

func (d *criDiscoverer) containerStatus(ctx context.Context, id string) (*runtime.ContainerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	resp, err := d.client.ContainerStatus(ctx, &runtime.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		return nil, err
	}
	return resp.Status, nil
}

func declaredPorts(containers []*runtime.Container) []declaredPort {
	var out []declaredPort
	names := map[string][]containerPort{}
	for _, c := range containers {
		if raw := c.Annotations[criContainerPorts]; raw != "" {
			var ports []containerPort
			if err := json.Unmarshal([]byte(raw), &ports); err == nil {
				names[c.Metadata.Name] = ports
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		for _, p := range names[n] {
			if p.Protocol == "" || p.Protocol == "TCP" {
				out = append(out, declaredPort{container: n, port: p})
			}
		}
	}
	return out
}

// expandTargets turns the pod's annotations and declared ports into targets
// (namespace/pod/host/key filled in by the caller).
func expandTargets(ann map[string]string, declared []declaredPort) []target {
	byPort := map[int]target{}
	order := []int{}
	add := func(t target) {
		if _, seen := byPort[t.port]; !seen {
			order = append(order, t.port)
		}
		byPort[t.port] = t
	}

	// 1. classic prometheus.io, lowest precedence
	if truthy(ann[annScrape]) {
		base := target{
			scheme: orDefault(ann[annScheme], "http"),
			path:   orDefault(ann[annPath], defaultPath),
		}
		portAnn := strings.TrimSpace(ann[annPort])
		switch {
		case portAnn == "":
			// every declared container port, like Prometheus's own
			// kubernetes_sd fallback
			for _, dp := range declared {
				t := base
				t.container = dp.container
				t.port = dp.port.ContainerPort
				add(t)
			}
		default:
			if n, err := strconv.Atoi(portAnn); err == nil && n > 0 {
				t := base
				t.port = n
				t.container = containerDeclaring(declared, n)
				add(t)
			} else if dp, ok := portByName(declared, portAnn); ok {
				t := base
				t.container = dp.container
				t.port = dp.port.ContainerPort
				add(t)
			}
		}
	}

	// 2. OpenTelemetry hints; port-scoped entries override anything above.
	// A port's hint is its port-scoped fields over the pod-level ones, field
	// by field, the key scoped by number over the one scoped by its name.
	// The candidates are the declared ports when the pod level enables them,
	// and every port a port-scoped entry enables or disables.
	pod, ports := otelHints(ann)
	byName := map[int]otelHint{}
	byNumber := map[int]otelHint{}
	at := map[int]declaredPort{}
	if pod.enabled != nil && *pod.enabled {
		for _, dp := range declared {
			if _, seen := at[dp.port.ContainerPort]; !seen {
				at[dp.port.ContainerPort] = dp
			}
		}
	}
	specs := make([]string, 0, len(ports))
	for spec := range ports {
		specs = append(specs, spec)
	}
	sort.Strings(specs)
	for _, spec := range specs {
		h := ports[spec]
		var dp declaredPort
		if n, err := strconv.Atoi(spec); err == nil && n > 0 {
			byNumber[n] = h
			dp = declaredPort{container: containerDeclaring(declared, n), port: containerPort{ContainerPort: n}}
		} else if named, ok := portByName(declared, spec); ok {
			if _, seen := byName[named.port.ContainerPort]; seen {
				continue
			}
			byName[named.port.ContainerPort] = h
			dp = named
		} else {
			continue
		}
		if _, seen := at[dp.port.ContainerPort]; !seen && h.enabled != nil {
			at[dp.port.ContainerPort] = dp
		}
	}
	for port, dp := range at {
		h := pod
		if s, ok := byName[port]; ok {
			h = s.over(h)
		}
		if s, ok := byNumber[port]; ok {
			h = s.over(h)
		}
		if h.enabled == nil || !*h.enabled || !scraperIsPrometheus(h.scraper) {
			continue
		}
		add(otelTarget(dp, h))
	}

	out := make([]target, 0, len(byPort))
	sort.Ints(order)
	for _, p := range order {
		out = append(out, byPort[p])
	}
	return out
}

// over is h with the fields it leaves unset taken from base.
func (h otelHint) over(base otelHint) otelHint {
	if h.enabled == nil {
		h.enabled = base.enabled
	}
	if h.scraper == "" {
		h.scraper = base.scraper
	}
	h.config = h.config.over(base.config)
	return h
}

func (c otelConfig) over(base otelConfig) otelConfig {
	if c.MetricsPath == "" {
		c.MetricsPath = base.MetricsPath
	}
	if c.CollectionInterval == "" {
		c.CollectionInterval = base.CollectionInterval
	}
	if c.MetricRelabelConfigs.Kind == 0 {
		c.MetricRelabelConfigs = base.MetricRelabelConfigs
	}
	return c
}

func otelTarget(dp declaredPort, h otelHint) target {
	t := target{
		container: dp.container,
		port:      dp.port.ContainerPort,
		scheme:    "http",
		path:      orDefault(h.config.MetricsPath, defaultPath),
	}
	if h.config.CollectionInterval != "" {
		if d, err := time.ParseDuration(h.config.CollectionInterval); err == nil && d > 0 {
			t.interval = d
		}
	}
	cfgs, err := parseRelabel(h.config.MetricRelabelConfigs)
	if err != nil {
		t.relabelErr = err.Error()
	} else {
		t.relabel = cfgs
	}
	return t
}

// otelHints groups io.opentelemetry.discovery.metrics[.<port>]/* annotations
// into the pod-level hint and a map keyed by the <port> spec (number or name).
func otelHints(ann map[string]string) (otelHint, map[string]otelHint) {
	pod := otelHint{}
	ports := map[string]otelHint{}
	for k, v := range ann {
		m := otelKeyRe.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		spec, field := m[1], m[2]
		h := pod
		if spec != "" {
			h = ports[spec]
		}
		switch field {
		case "enabled":
			b := truthy(v)
			h.enabled = &b
		case "scraper":
			h.scraper = strings.TrimSpace(v)
		case "config":
			// unknown fields (endpoint, tls, ...) are intentionally ignored:
			// the endpoint is always pod IP + port here
			_ = yaml.Unmarshal([]byte(v), &h.config)
		}
		if spec == "" {
			pod = h
		} else {
			ports[spec] = h
		}
	}
	return pod, ports
}

func scraperIsPrometheus(s string) bool {
	switch s {
	case "", "prometheus", "prometheus_simple":
		return true
	}
	return false
}

func containerDeclaring(declared []declaredPort, port int) string {
	for _, dp := range declared {
		if dp.port.ContainerPort == port {
			return dp.container
		}
	}
	return ""
}

func portByName(declared []declaredPort, name string) (declaredPort, bool) {
	for _, dp := range declared {
		if dp.port.Name == name {
			return dp, true
		}
	}
	return declaredPort{}, false
}

// sandboxFacts are what the sandbox status adds to the list entry: the pod
// IP (scrape targets, podindex) and the QoS class (podindex).
type sandboxFacts struct {
	podIP    string
	qosClass string
}

// sandboxStatus makes one verbose PodSandboxStatus call per sandbox and
// caches the result for the sandbox's lifetime. The verbose info is
// containerd's JSON of the sandbox, whose config carries the cgroup parent
// the kubelet chose, and with it the QoS class.
func (d *criDiscoverer) sandboxStatus(ctx context.Context, sandboxID string) (sandboxFacts, error) {
	if f, ok := d.sbCache[sandboxID]; ok {
		return f, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	st, err := d.client.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{PodSandboxId: sandboxID, Verbose: true})
	if err != nil {
		return sandboxFacts{}, fmt.Errorf("PodSandboxStatus: %w", err)
	}
	f := sandboxFacts{}
	if st.Status != nil && st.Status.Network != nil {
		f.podIP = st.Status.Network.Ip
	}
	var info struct {
		Config struct {
			Linux struct {
				CgroupParent string `json:"cgroup_parent"`
			} `json:"linux"`
		} `json:"config"`
	}
	if raw := st.Info["info"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &info); err == nil {
			f.qosClass = qosClass(info.Config.Linux.CgroupParent)
		}
	}
	d.sbCache[sandboxID] = f
	return f, nil
}

// qosClass derives the pod QoS class from the sandbox cgroup parent:
// /kubepods/besteffort/pod<uid>, /kubepods/burstable/pod<uid> and
// /kubepods/pod<uid> (Guaranteed) with the cgroupfs driver, and the
// kubepods-besteffort.slice, kubepods-burstable.slice and kubepods-pod<uid>.slice
// equivalents with the systemd driver. Anything else is unknown ("").
func qosClass(cgroupParent string) string {
	switch {
	case strings.Contains(cgroupParent, "besteffort"):
		return "besteffort"
	case strings.Contains(cgroupParent, "burstable"):
		return "burstable"
	case strings.Contains(cgroupParent, "kubepods"):
		return "guaranteed"
	}
	return ""
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
