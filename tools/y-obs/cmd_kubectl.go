package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const kubectlUsage = `Usage: y-obs kubectl --at TIME [flags] [-- KUBECTL ARGS]

kubectl against the cluster as it was at TIME (UTC), read-only: the
cluster agent's objects from the bucket (K8SAGENT.md, "Reading") served by
a stock kube-apiserver of the cluster's release. Without kubectl args it
keeps serving and prints the KUBECONFIG to use, until interrupted.

The objects come from the hourly tier, the hour of TIME and the two before
(a snapshot's rows can sit in the hour before its own), else the daily
tier, the day of TIME and the day before. --source raw reads the raw tier
of those hours instead, for times the tiers do not have yet: the cluster
agent's objects only (node _k8sagent), unless --node says otherwise.

The API server and etcd are the cluster's release at TIME, from its Nodes'
kubelet version: --assets or KUBEBUILDER_ASSETS, else setup-envtest
(on PATH, or with go) downloads them.

Examples:
  y-obs kubectl --cluster prod --at '2026-10-11 09:00' -- get pods -A
  y-obs kubectl --cluster prod --at '2026-10-11 09:00' -- describe deploy -n qa web
  y-obs kubectl --cluster prod --at '2026-10-11 11:58' --source raw -- get events -n qa

Flags:
`

// k8sAgentNode is the node segment of the cluster agent's raw keys.
const k8sAgentNode = "_k8sagent"

// setupEnvtest is the version local/kubectl-at.sh pins.
const setupEnvtest = "v0.25.2"

func cmdKubectl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kubectl", flag.ContinueOnError)
	var sf sessionFlags
	sf.register(fs)
	at := fs.String("at", "", "the time, UTC: 2026-10-11T09:00:00Z or '2026-10-11 09:00'")
	source := fs.String("source", "tier", "tier (hourly, else daily) or raw")
	assets := fs.String("assets", os.Getenv("KUBEBUILDER_ASSETS"), "directory with etcd and kube-apiserver of the cluster's release (env KUBEBUILDER_ASSETS)")
	verbose := fs.Bool("v", false, "pass through the API server's and etcd's output")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), kubectlUsage)
		fs.PrintDefaults()
	}
	var kargs []string
	for i, a := range args {
		if a == "--" {
			args, kargs = args[:i], args[i+1:]
			break
		}
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK}
		}
		return fail(exitUsage, "%v; run y-obs kubectl -h", err)
	}
	if fs.NArg() > 0 {
		return fail(exitUsage, "kubectl's arguments go after --: y-obs kubectl --at T -- %s", strings.Join(fs.Args(), " "))
	}
	if *at == "" {
		return fail(exitUsage, "--at is required: the time to read the cluster at, UTC")
	}
	t, err := parseTime(*at)
	if err != nil {
		return fail(exitUsage, "--at: %v", err)
	}
	lines, minor, err := stateAt(ctx, &sf, t, *source)
	if err != nil {
		return err
	}
	if *assets == "" {
		if *assets, err = envtestAssets(ctx, minor); err != nil {
			return err
		}
	}
	p, err := Prepare(lines)
	if err != nil {
		return fail(exitStore, "the state at %s: %v", t.Format(time.RFC3339), err)
	}
	for reason, n := range p.Skipped {
		fmt.Fprintf(os.Stderr, "y-obs: left out %d objects: %s\n", n, reason)
	}
	o := Options{Assets: *assets, Prepared: p, Listen: "127.0.0.1:0", Label: t.Format("2006-01-02T15:04:05Z") + " UTC"}
	if *verbose {
		o.Log = os.Stderr
	}
	srv, err := Start(ctx, o)
	if err != nil {
		return fail(exitStore, "the API server at %s: %v", o.Label, err)
	}
	defer srv.Stop()
	dir, err := os.MkdirTemp("", "y-obs-kubectl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	kc, err := srv.KubeConfig(sf.cluster + "-at")
	if err != nil {
		return err
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "y-obs: %s at %s, %d objects, read-only\n", sf.cluster, o.Label, len(p.KVs))
	if len(kargs) == 0 {
		fmt.Fprintf(os.Stderr, "export KUBECONFIG=%s    # until ^C\n", kubeconfig)
		<-ctx.Done()
		return nil
	}
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, kargs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitError{code: ee.ExitCode()}
		}
		return fail(exitUsage, "kubectl: %v", err)
	}
	return nil
}

// stateAt reads the objects of local/apiserver_at.sql at t, and the
// minor release the cluster's kubelets reported then (1.37), "" without
// Nodes.
func stateAt(ctx context.Context, sf *sessionFlags, t time.Time, source string) ([]Line, string, error) {
	hour := t.Truncate(time.Hour)
	hours := window{from: hour.Add(-2 * time.Hour), to: hour.Add(time.Hour)}
	day := t.Truncate(24 * time.Hour)
	days := window{from: day.Add(-24 * time.Hour), to: day.Add(24 * time.Hour)}
	var s *session
	var err error
	switch source {
	case "raw":
		// apiserver rows come only from the cluster agent, whose keys use
		// _k8sagent as the node (K8SAGENT.md): the nodes' objects are skipped
		if sf.node == "" {
			raw := *sf
			raw.node = k8sAgentNode
			sf = &raw
		}
		if s, err = openSession(ctx, sf, hours); err != nil {
			return nil, "", err
		}
		if err := s.prepare(ctx, map[string]bool{"raw": true}, sf.maxObjects); err != nil {
			s.close()
			return nil, "", err
		}
		if len(s.plan["raw"]) == 0 {
			s.close()
			return nil, "", fail(exitNoRows, "no raw objects of %s in %s; the tiers may have them: drop --source raw", sf.cluster, hours)
		}
	case "tier":
		for _, c := range []struct {
			w    window
			view string
		}{{hours, "hourly_apiserver"}, {days, "daily_apiserver"}} {
			if s, err = openSession(ctx, sf, c.w); err != nil {
				return nil, "", err
			}
			if err := s.prepare(ctx, map[string]bool{c.view: true}, sf.maxObjects); err != nil {
				s.close()
				return nil, "", err
			}
			if len(s.plan[c.view]) > 0 {
				if err := s.exec(ctx, "CREATE OR REPLACE VIEW apiserver AS SELECT * FROM "+c.view); err != nil {
					s.close()
					return nil, "", err
				}
				break
			}
			s.close()
			s = nil
		}
		if s == nil {
			return nil, "", fail(exitNoRows, "no apiserver objects of %s around %s in the hourly or daily tier; --source raw reads raw1/", sf.cluster, t.Format(time.RFC3339))
		}
	default:
		return nil, "", fail(exitUsage, "--source %q: tier or raw", source)
	}
	defer s.close()
	if sf.explain {
		s.printPlan(os.Stdout)
		return nil, "", &exitError{code: exitOK}
	}
	ts := sqlString(t.Format("2006-01-02 15:04:05.000000000"))
	rows, err := s.conn.QueryContext(ctx, "SELECT api_version, resource, coalesce(namespace, ''), name, apijson FROM apiserver_at(TIMESTAMP_NS "+ts+")")
	if err != nil {
		return nil, "", s.wrap(err)
	}
	defer rows.Close()
	var lines []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.APIVersion, &l.Resource, &l.Namespace, &l.Name, &l.Object); err != nil {
			return nil, "", err
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, "", s.wrap(err)
	}
	if len(lines) == 0 {
		return nil, "", fail(exitNoRows, "no complete snapshot of %s at or before %s (%s)", sf.cluster, t.Format(time.RFC3339), s.planSummary())
	}
	var minor string
	err = s.conn.QueryRowContext(ctx, `SELECT coalesce(regexp_extract(max(json_extract_string(apijson, '$.status.nodeInfo.kubeletVersion')), '^v([0-9]+\.[0-9]+)', 1), '')
		FROM apiserver_at(TIMESTAMP_NS `+ts+`) WHERE api_version = 'v1' AND resource = 'nodes'`).Scan(&minor)
	if err != nil {
		return nil, "", s.wrap(err)
	}
	return lines, minor, nil
}

// envtestAssets downloads etcd and kube-apiserver of a minor release with
// setup-envtest, from PATH or with go, and returns their directory.
func envtestAssets(ctx context.Context, minor string) (string, error) {
	if minor == "" {
		return "", fail(exitUsage, "no Node with a kubelet version in the state, so no release to run: pass --assets or set KUBEBUILDER_ASSETS")
	}
	var cmd *exec.Cmd
	if p, err := exec.LookPath("setup-envtest"); err == nil {
		cmd = exec.CommandContext(ctx, p, "use", minor+".x", "-p", "path")
	} else if g, err := exec.LookPath("go"); err == nil {
		cmd = exec.CommandContext(ctx, g, "run", "sigs.k8s.io/controller-runtime/tools/setup-envtest@"+setupEnvtest, "use", minor+".x", "-p", "path")
		cmd.Env = append(os.Environ(), "GOFLAGS=")
	} else {
		return "", fail(exitUsage, "Kubernetes %s.x binaries are needed: pass --assets, set KUBEBUILDER_ASSETS, or put setup-envtest or go on PATH", minor)
	}
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return "", fail(exitStore, "setup-envtest use %s.x: %v", minor, err)
	}
	return strings.TrimSpace(string(out)), nil
}
