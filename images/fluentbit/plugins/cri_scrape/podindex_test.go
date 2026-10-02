package main

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func sandbox(uid, ns, name string, created time.Time, labels map[string]string) *runtime.PodSandbox {
	return &runtime.PodSandbox{
		Id:        "sb-" + uid,
		Metadata:  &runtime.PodSandboxMetadata{Uid: uid, Namespace: ns, Name: name},
		CreatedAt: created.UnixNano(),
		Labels:    labels,
	}
}

func criContainer(id, name string, restart int, state runtime.ContainerState) *runtime.Container {
	return &runtime.Container{
		Id:          id,
		Metadata:    &runtime.ContainerMetadata{Name: name},
		State:       state,
		Image:       &runtime.ImageSpec{Image: "sha256:db287cb6be81", UserSpecifiedImage: "busybox:1.37"},
		Annotations: map[string]string{criRestartCount: strconv.Itoa(restart)},
	}
}

func TestCriContainerRestartCount(t *testing.T) {
	if r := containerRestart(criContainer("c", "app", 12, runtime.ContainerState_CONTAINER_RUNNING)); r != 12 {
		t.Fatalf("restart %d, want 12", r)
	}
}

func collect(rows *[]pirow) func([]pirow) {
	return func(r []pirow) { *rows = append(*rows, r...) }
}

func kinds(rows []pirow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Kind)
	}
	return out
}

func TestPodindexLifecycle(t *testing.T) {
	var rows []pirow
	tr := newPodindexTracker(time.Hour, collect(&rows))
	now := time.Now()
	sb := sandbox("uid1", "default", "p1", now.Add(-time.Minute), map[string]string{
		"app": "x", "io.kubernetes.pod.name": "p1",
	})
	cs := map[string][]*runtime.Container{"sb-uid1": {criContainer("c1", "app", 0, runtime.ContainerState_CONTAINER_RUNNING)}}

	sandboxCalls, containerCalls := 0, 0
	started := now.Add(-50 * time.Second)
	status := criStatus{
		sandbox: func(_ context.Context, id string) (sandboxFacts, error) {
			sandboxCalls++
			return sandboxFacts{podIP: "10.42.0.7", qosClass: "burstable"}, nil
		},
		container: func(_ context.Context, id string) (*runtime.ContainerStatus, error) {
			containerCalls++
			return &runtime.ContainerStatus{
				ImageRef:  "docker.io/library/busybox@sha256:9db7",
				StartedAt: started.UnixNano(),
				Resources: &runtime.ContainerResources{Linux: &runtime.LinuxContainerResources{
					MemoryLimitInBytes: 64 << 20, CpuShares: 2,
				}},
			}, nil
		},
	}
	tr.observe(context.Background(), now, []*runtime.PodSandbox{sb}, cs, status)
	if len(rows) != 2 || rows[0].Kind != "pod-observed" || rows[1].Kind != "container" {
		t.Fatalf("first round: %v", kinds(rows))
	}
	if rows[1].ContainerID != "c1" {
		t.Fatalf("container row: container id %q, want the CRI id c1", rows[1].ContainerID)
	}
	if rows[1].Image != "busybox:1.37" {
		t.Fatalf("image should be the user-specified ref: %+v", rows[1])
	}
	if rows[0].QOSClass != "burstable" || rows[0].PodIP != "10.42.0.7" {
		t.Fatalf("sandbox facts: %+v", rows[0])
	}
	c := rows[1]
	if c.ImageID != "docker.io/library/busybox@sha256:9db7" || c.ContainerStarted == nil ||
		!c.ContainerStarted.Equal(started) || c.MemoryLimit != 64<<20 || c.CPUQuota != 0 || c.CPUShares != 2 {
		t.Fatalf("container status facts: %+v", c)
	}
	if c.ExitCode != nil || c.ExitMessage != "" {
		t.Fatalf("container row carries exit fields: %+v", c)
	}
	if len(rows[0].Labels) != 1 || rows[0].Labels["app"] != "x" {
		t.Fatalf("labels not filtered: %+v", rows[0].Labels)
	}
	if rows[0].PodStarted == nil || !rows[0].PodStarted.Equal(now.Add(-time.Minute)) {
		t.Fatalf("pod_started: %+v", rows[0].PodStarted)
	}

	// same state again: nothing new, and no status calls
	rows = nil
	tr.observe(context.Background(), now.Add(10*time.Second), []*runtime.PodSandbox{sb}, cs, status)
	if len(rows) != 0 {
		t.Fatalf("idempotent round emitted %v", kinds(rows))
	}
	if sandboxCalls != 1 || containerCalls != 1 {
		t.Fatalf("status calls: sandbox %d, container %d", sandboxCalls, containerCalls)
	}

	// restart count grows and an exited incarnation appears
	code := int32(1)
	status.container = func(_ context.Context, id string) (*runtime.ContainerStatus, error) {
		if id == "c2" {
			return &runtime.ContainerStatus{StartedAt: now.Add(6 * time.Second).UnixNano()}, nil
		}
		return &runtime.ContainerStatus{ExitCode: code, Reason: "Error", Message: "boom",
			FinishedAt: now.Add(5 * time.Second).UnixNano()}, nil
	}
	cs["sb-uid1"] = []*runtime.Container{
		criContainer("c1", "app", 0, runtime.ContainerState_CONTAINER_EXITED),
		criContainer("c2", "app", 1, runtime.ContainerState_CONTAINER_RUNNING),
	}
	rows = nil
	tr.observe(context.Background(), now.Add(20*time.Second), []*runtime.PodSandbox{sb}, cs, status)
	if len(rows) != 2 {
		t.Fatalf("restart round: %v", kinds(rows))
	}
	// container order within a pod is CRI-defined; find rows by kind
	byKind := map[string]pirow{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	if r := byKind["container"]; r.Restart != 1 || r.ContainerStarted == nil ||
		!r.ContainerStarted.Equal(now.Add(6*time.Second)) || r.ImageID != "" || r.MemoryLimit != 0 {
		t.Fatalf("restart row: %+v", r)
	}
	if r := byKind["container"]; r.ContainerID != "c2" {
		t.Fatalf("restart row: container id %q, want the new run's c2", r.ContainerID)
	}
	exit := byKind["container-exit"]
	if exit.ContainerID != "c1" {
		t.Fatalf("exit row: container id %q, want the exited run's c1", exit.ContainerID)
	}
	if exit.ExitCode == nil || *exit.ExitCode != 1 || exit.ExitReason != "Error" || exit.ExitMessage != "boom" {
		t.Fatalf("exit row: %+v", exit)
	}
	if exit.ContainerStarted != nil || exit.ImageID != "" {
		t.Fatalf("exit row carries container-row fields: %+v", exit)
	}
	if !exit.Time.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("exit time should be FinishedAt: %v", exit.Time)
	}

	// pod disappears
	rows = nil
	tr.observe(context.Background(), now.Add(30*time.Second), nil, nil, criStatus{})
	if len(rows) != 1 || rows[0].Kind != "pod-deleted" || rows[0].PodUID != "uid1" {
		t.Fatalf("delete round: %v", kinds(rows))
	}
}

// A pod that completes leaves the ready list before the next round; its
// exited containers are still listed, and the exit must be recorded
// before the deletion.
func TestPodindexCompletedPodExitObserved(t *testing.T) {
	var rows []pirow
	tr := newPodindexTracker(time.Hour, collect(&rows))
	now := time.Now()
	sb := sandbox("uid1", "default", "job-1", now, nil)
	cs := map[string][]*runtime.Container{"sb-uid1": {criContainer("c1", "step", 0, runtime.ContainerState_CONTAINER_RUNNING)}}
	status := criStatus{container: func(_ context.Context, id string) (*runtime.ContainerStatus, error) {
		return &runtime.ContainerStatus{ExitCode: 0, Reason: "Completed", FinishedAt: now.Add(5 * time.Second).UnixNano()}, nil
	}}
	tr.observe(context.Background(), now, []*runtime.PodSandbox{sb}, cs, status)
	rows = nil
	cs["sb-uid1"] = []*runtime.Container{criContainer("c1", "step", 0, runtime.ContainerState_CONTAINER_EXITED)}
	tr.observe(context.Background(), now.Add(10*time.Second), nil, cs, status)
	if len(rows) != 2 || rows[0].Kind != "container-exit" || rows[1].Kind != "pod-deleted" {
		t.Fatalf("completed pod: %v", kinds(rows))
	}
	if rows[0].ExitCode == nil || *rows[0].ExitCode != 0 || rows[0].ExitReason != "Completed" {
		t.Fatalf("exit row: %+v", rows[0])
	}
}

func TestPodindexCheckpoint(t *testing.T) {
	var rows []pirow
	tr := newPodindexTracker(time.Minute, collect(&rows))
	now := time.Now()
	tr.lastSweep = now
	sb := sandbox("uid1", "ns", "p1", now, nil)
	tr.observe(context.Background(), now, []*runtime.PodSandbox{sb}, nil, criStatus{})
	rows = nil
	tr.observe(context.Background(), now.Add(61*time.Second), []*runtime.PodSandbox{sb}, nil, criStatus{})
	if len(rows) != 1 || rows[0].Kind != "pod-checkpoint" {
		t.Fatalf("checkpoint: %v", kinds(rows))
	}
	rows = nil
	tr.observe(context.Background(), now.Add(90*time.Second), []*runtime.PodSandbox{sb}, nil, criStatus{})
	if len(rows) != 0 {
		t.Fatalf("early checkpoint: %v", kinds(rows))
	}
}

func TestQOSClass(t *testing.T) {
	for in, want := range map[string]string{
		"/kubepods/besteffort/pod0f1e":      "besteffort",
		"/kubepods/burstable/pod0f1e":       "burstable",
		"/kubepods/pod0f1e":                 "guaranteed",
		"kubepods-besteffort-pod0f1e.slice": "besteffort",
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod0f1e.slice": "burstable",
		"kubepods-pod0f1e.slice": "guaranteed",
		"":                       "",
		"/system.slice/other":    "",
	} {
		if got := qosClass(in); got != want {
			t.Errorf("qosClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed sandbox status at first sighting is retried on later rounds,
// without extra rows, so that later rows carry the facts.
func TestPodindexSandboxFactsRetried(t *testing.T) {
	var rows []pirow
	tr := newPodindexTracker(time.Minute, collect(&rows))
	now := time.Now()
	tr.lastSweep = now
	sb := sandbox("uid1", "ns", "p1", now, nil)
	calls := 0
	status := criStatus{sandbox: func(_ context.Context, id string) (sandboxFacts, error) {
		calls++
		if calls == 1 {
			return sandboxFacts{}, errors.New("deadline exceeded")
		}
		return sandboxFacts{podIP: "10.42.0.7", qosClass: "besteffort"}, nil
	}}
	tr.observe(context.Background(), now, []*runtime.PodSandbox{sb}, nil, status)
	if len(rows) != 1 || rows[0].Kind != "pod-observed" || rows[0].PodIP != "" {
		t.Fatalf("first round: %+v", rows)
	}
	rows = nil
	tr.observe(context.Background(), now.Add(10*time.Second), []*runtime.PodSandbox{sb}, nil, status)
	if len(rows) != 0 {
		t.Fatalf("retry round emitted %v", kinds(rows))
	}
	tr.observe(context.Background(), now.Add(20*time.Second), []*runtime.PodSandbox{sb}, nil, status)
	if calls != 2 {
		t.Fatalf("sandbox status calls: %d, want 2", calls)
	}
	tr.observe(context.Background(), now.Add(61*time.Second), []*runtime.PodSandbox{sb}, nil, status)
	if len(rows) != 1 || rows[0].Kind != "pod-checkpoint" || rows[0].PodIP != "10.42.0.7" || rows[0].QOSClass != "besteffort" {
		t.Fatalf("checkpoint after retry: %+v", rows)
	}
}

// A failed container status on an exit is retried, so the exit code is not
// lost to one slow call; the exit is recorded without it once the
// container is no longer listed, or its pod is gone.
func TestPodindexExitStatusRetried(t *testing.T) {
	now := time.Now()
	sb := sandbox("uid1", "default", "p1", now, nil)
	running := map[string][]*runtime.Container{"sb-uid1": {criContainer("c1", "app", 0, runtime.ContainerState_CONTAINER_RUNNING)}}
	exited := map[string][]*runtime.Container{"sb-uid1": {criContainer("c1", "app", 0, runtime.ContainerState_CONTAINER_EXITED)}}
	removed := map[string][]*runtime.Container{"sb-uid1": {criContainer("c2", "app", 1, runtime.ContainerState_CONTAINER_RUNNING)}}
	fail := true
	status := criStatus{container: func(_ context.Context, id string) (*runtime.ContainerStatus, error) {
		if fail {
			return nil, errors.New("deadline exceeded")
		}
		return &runtime.ContainerStatus{ExitCode: 137, Reason: "OOMKilled", FinishedAt: now.Add(5 * time.Second).UnixNano()}, nil
	}}
	start := func() (*podindexTracker, *[]pirow) {
		var rows []pirow
		tr := newPodindexTracker(time.Hour, collect(&rows))
		tr.lastSweep = now
		tr.observe(context.Background(), now, []*runtime.PodSandbox{sb}, running, status)
		rows = nil
		fail = true
		tr.observe(context.Background(), now.Add(10*time.Second), []*runtime.PodSandbox{sb}, exited, status)
		if len(rows) != 0 {
			t.Fatalf("exit emitted although its status failed: %+v", rows)
		}
		return tr, &rows
	}

	tr, rows := start()
	fail = false
	tr.observe(context.Background(), now.Add(20*time.Second), []*runtime.PodSandbox{sb}, exited, status)
	if len(*rows) != 1 || (*rows)[0].Kind != "container-exit" || (*rows)[0].ExitCode == nil || *(*rows)[0].ExitCode != 137 {
		t.Fatalf("retry: %+v", *rows)
	}
	*rows = nil
	tr.observe(context.Background(), now.Add(30*time.Second), []*runtime.PodSandbox{sb}, exited, status)
	if len(*rows) != 0 {
		t.Fatalf("exit emitted twice: %+v", *rows)
	}

	// removed before the status call succeeded: recorded without it
	tr, rows = start()
	tr.observe(context.Background(), now.Add(20*time.Second), []*runtime.PodSandbox{sb}, removed, status)
	byKind := map[string]pirow{}
	for _, r := range *rows {
		byKind[r.Kind] = r
	}
	exit, ok := byKind["container-exit"]
	if len(*rows) != 2 || !ok || exit.ContainerID != "c1" || exit.ExitCode != nil || !exit.Time.Equal(now.Add(10*time.Second)) {
		t.Fatalf("removed container: %+v", *rows)
	}
	*rows = nil
	tr.observe(context.Background(), now.Add(30*time.Second), []*runtime.PodSandbox{sb}, removed, status)
	if len(*rows) != 0 {
		t.Fatalf("exit emitted twice: %+v", *rows)
	}

	// pod gone while the exited container is still listed and failing
	tr, rows = start()
	tr.observe(context.Background(), now.Add(20*time.Second), nil, exited, status)
	if got := kinds(*rows); len(got) != 2 || got[0] != "container-exit" || got[1] != "pod-deleted" {
		t.Fatalf("deleted pod: %v", got)
	}
}
