package main

// podindex: pod and container lifecycle events as the CRI shows them, with
// no API-server access. Append-only rows, re-emitted freely (an agent
// restart re-observes everything); the compactor deduplicates into one row
// per incarnation (pod_uid) and container.
//
// Row kinds:
//
//	pod-observed      first sighting of a pod (sandbox) by this process
//	pod-checkpoint    periodic attestation of live pods
//	pod-deleted       the sandbox left the ready list (terminated or gone)
//	container         a container was seen (first time, or restart count grew)
//	container-exit    a container exit, with code/reason at CRI-status precision
//
// Pod rows carry the QoS class and pod IP from one verbose sandbox status
// call per new sandbox (retried while it fails); container rows carry the resolved image digest,
// start time and resource limits from one container status call per
// container start. Owner references and evictions are not visible from
// the CRI.

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const criRestartCount = "io.kubernetes.container.restartCount"

// pirow is one podindex record; nil pointers, empty strings and zero limits
// are fields the record does not carry.
type pirow struct {
	Time      time.Time
	Kind      string
	Namespace string
	Pod       string
	PodUID    string
	Container string
	// the runtime's id of this run of the container, bare: the API's
	// containerStatuses[].containerID without its <runtime>:// prefix
	ContainerID string
	Image       string
	Labels      map[string]string
	PodStarted  *time.Time // sandbox creation: constant across container restarts
	QOSClass    string
	PodIP       string
	Restart     int32
	// from the container status, on container rows
	ImageID          string
	ContainerStarted *time.Time
	MemoryLimit      int64
	CPUQuota         int64
	CPUShares        int64
	// from the container status, on container-exit rows
	ExitCode    *int32
	ExitReason  string
	ExitMessage string
}

// criStatus resolves what the list calls do not return. Either func may be
// nil (the rows then lack those fields).
type criStatus struct {
	container func(ctx context.Context, containerID string) (*runtime.ContainerStatus, error)
	sandbox   func(ctx context.Context, sandboxID string) (sandboxFacts, error)
}

type piContainer struct {
	restart      int32
	exitsEmitted map[string]bool // by container id
	// exits whose container status failed, retried each round, by
	// container id; the row as first observed, without status fields
	exitsPending map[string]pirow
}

type piPod struct {
	sandboxID  string
	namespace  string
	pod        string
	labels     map[string]string
	started    time.Time
	facts      sandboxFacts
	factsOK    bool // false until a sandbox status call succeeds
	containers map[string]*piContainer
}

type podindexTracker struct {
	emit       func([]pirow)
	checkpoint time.Duration
	lastSweep  time.Time
	pods       map[string]*piPod // by pod uid
}

func newPodindexTracker(checkpoint time.Duration, emit func([]pirow)) *podindexTracker {
	return &podindexTracker{
		emit:       emit,
		checkpoint: checkpoint,
		pods:       map[string]*piPod{},
	}
}

// observe consumes one discovery round: ready sandboxes and their containers
// in every state. status is called for sandboxes whose facts are missing
// and for containers that get a container or container-exit row.
func (t *podindexTracker) observe(ctx context.Context, now time.Time,
	sandboxes []*runtime.PodSandbox, containers map[string][]*runtime.Container, status criStatus) {
	var rows []pirow
	live := map[string]bool{}
	sweep := now.Sub(t.lastSweep) >= t.checkpoint

	for _, sb := range sandboxes {
		if sb.Metadata == nil {
			continue
		}
		uid := sb.Metadata.Uid
		live[uid] = true
		p, known := t.pods[uid]
		if !known {
			p = &piPod{
				sandboxID:  sb.Id,
				namespace:  sb.Metadata.Namespace,
				pod:        sb.Metadata.Name,
				labels:     podLabels(sb.Labels),
				started:    time.Unix(0, sb.CreatedAt),
				containers: map[string]*piContainer{},
			}
			t.pods[uid] = p
		}
		// retried each round until it succeeds, so that a failed call at
		// first sighting does not leave the pod's later rows without facts
		if !p.factsOK && status.sandbox != nil {
			if f, err := status.sandbox(ctx, sb.Id); err == nil {
				p.facts, p.factsOK = f, true
			} else {
				log.Printf("[cri_scrape] podindex sandbox status %s/%s: %v", p.namespace, p.pod, err)
			}
		}
		if !known {
			rows = append(rows, t.podRow("pod-observed", now, uid, p))
		} else if sweep {
			rows = append(rows, t.podRow("pod-checkpoint", now, uid, p))
		}
		p.sandboxID = sb.Id
		rows = append(rows, t.observeContainers(ctx, now, uid, p, containers[sb.Id], status)...)
	}

	for uid, p := range t.pods {
		if !live[uid] {
			// The sandbox left the ready list, but its exited containers are
			// still listed until the kubelet removes them: a completed pod's
			// last exit is observed here, before the deletion.
			rows = append(rows, t.observeContainers(ctx, now, uid, p, containers[p.sandboxID], status)...)
			rows = append(rows, p.pendingExits(nil)...)
			rows = append(rows, t.podRow("pod-deleted", now, uid, p))
			delete(t.pods, uid)
		}
	}
	if sweep {
		t.lastSweep = now
	}
	if len(rows) > 0 {
		t.emit(rows)
	}
}

// observeContainers emits container and container-exit rows for what is new
// about a pod's containers since the last round.
func (t *podindexTracker) observeContainers(ctx context.Context, now time.Time, uid string, p *piPod,
	cs []*runtime.Container, status criStatus) []pirow {
	var rows []pirow
	listed := map[string]bool{}
	for _, c := range cs {
		if c.Metadata == nil {
			continue
		}
		listed[c.Id] = true
		name := c.Metadata.Name
		restart := containerRestart(c)
		pc, seen := p.containers[name]
		if !seen {
			pc = &piContainer{restart: -1, exitsEmitted: map[string]bool{}, exitsPending: map[string]pirow{}}
			p.containers[name] = pc
		}
		newRestart := restart > pc.restart
		newExit := c.State == runtime.ContainerState_CONTAINER_EXITED && !pc.exitsEmitted[c.Id]
		if !newRestart && !newExit {
			continue
		}
		var st *runtime.ContainerStatus
		if status.container != nil {
			var err error
			if st, err = status.container(ctx, c.Id); err != nil {
				log.Printf("[cri_scrape] podindex container status %s/%s: %v", p.pod, name, err)
			}
		}
		if newRestart {
			pc.restart = restart
			row := pirow{
				Time: now, Kind: "container",
				Namespace: p.namespace, Pod: p.pod, PodUID: uid,
				Container: name, ContainerID: c.Id, Image: containerImage(c), Restart: restart,
			}
			if st != nil {
				row.ImageID = st.ImageRef
				if st.StartedAt > 0 {
					started := time.Unix(0, st.StartedAt)
					row.ContainerStarted = &started
				}
				if r := st.GetResources().GetLinux(); r != nil {
					row.MemoryLimit = r.MemoryLimitInBytes
					row.CPUQuota = r.CpuQuota
					row.CPUShares = r.CpuShares
				}
			}
			rows = append(rows, row)
		}
		if newExit {
			row := pirow{
				Time: now, Kind: "container-exit",
				Namespace: p.namespace, Pod: p.pod, PodUID: uid,
				Container: name, ContainerID: c.Id, Image: containerImage(c), Restart: restart,
			}
			if st == nil && status.container != nil {
				// the exit code is retried next round; pendingExits records
				// the exit without it if the container is removed first
				if _, ok := pc.exitsPending[c.Id]; !ok {
					pc.exitsPending[c.Id] = row
				}
				continue
			}
			pc.exitsEmitted[c.Id] = true
			delete(pc.exitsPending, c.Id)
			if st != nil {
				code := st.ExitCode
				row.ExitCode = &code
				row.ExitReason = st.Reason
				row.ExitMessage = st.Message
				if st.FinishedAt > 0 {
					row.Time = time.Unix(0, st.FinishedAt)
				}
			}
			rows = append(rows, row)
		}
	}
	return append(rows, p.pendingExits(listed)...)
}

// pendingExits gives up on the exits whose container status never
// succeeded and whose container is no longer listed (all of them for a nil
// listed), returning their rows without the status fields.
func (p *piPod) pendingExits(listed map[string]bool) []pirow {
	var rows []pirow
	for _, pc := range p.containers {
		for id, row := range pc.exitsPending {
			if listed[id] {
				continue
			}
			pc.exitsEmitted[id] = true
			delete(pc.exitsPending, id)
			rows = append(rows, row)
		}
	}
	return rows
}

func (t *podindexTracker) podRow(kind string, now time.Time, uid string, p *piPod) pirow {
	started := p.started
	return pirow{
		Time: now, Kind: kind,
		Namespace: p.namespace, Pod: p.pod, PodUID: uid,
		Labels: p.labels, PodStarted: &started,
		QOSClass: p.facts.qosClass, PodIP: p.facts.podIP,
	}
}

// podLabels filters the CRI sandbox label map down to the pod's own labels:
// the kubelet adds io.kubernetes.pod.* bookkeeping keys next to them.
func podLabels(criLabels map[string]string) map[string]string {
	out := make(map[string]string, len(criLabels))
	for k, v := range criLabels {
		if strings.HasPrefix(k, "io.kubernetes.") {
			continue
		}
		out[k] = v
	}
	return out
}

func containerRestart(c *runtime.Container) int32 {
	if n, err := strconv.Atoi(c.Annotations[criRestartCount]); err == nil {
		return int32(n)
	}
	return 0
}

// containerImage prefers the image as written in the pod spec; containerd
// resolves ImageSpec.Image to the sha256 id, which answers nothing a human
// asks the index.
func containerImage(c *runtime.Container) string {
	if c.Image != nil && c.Image.UserSpecifiedImage != "" {
		return c.Image.UserSpecifiedImage
	}
	if c.Image != nil && c.Image.Image != "" {
		return c.Image.Image
	}
	return c.ImageRef
}
