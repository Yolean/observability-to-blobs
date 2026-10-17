package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const statusUsage = `Usage: y-obs status [flags]

What y-obs can reach, and what is there:

  contexts    each context's bucket, cluster and AWS profile, the one in
              use marked *, and for that one the S3 endpoint and where
              its credential comes from and until when (README.md,
              "S3 access")
  prometheus  the running y-obs prometheus instances
  data        per cluster in the bucket in use and per tier (raw, hourly,
              daily): first and last hour or day, objects and bytes, the
              newest upload, and for raw the nodes that uploaded in its
              last hour

Raw objects are about 120 per node per hour, so raw's objects and bytes
are measured over its last 24 hours of data unless --full lists all of
it; hourly and daily are listed in full. Retention is the bucket's
lifecycle rules, which a read-only key cannot see: the spans say how far
back each tier reaches, not when it expires. Secrets are never printed.

Examples:
  y-obs status
  y-obs status --cluster prod --full
  y-obs status --format json

Flags:
`

type contextRow struct {
	Context    string `json:"context"`
	Current    bool   `json:"current"`
	Bucket     string `json:"bucket"`
	Cluster    string `json:"cluster"`
	Profile    string `json:"profile"`
	Endpoint   string `json:"endpoint,omitempty"`
	Credential string `json:"credential,omitempty"`
}

type instanceRow struct {
	Instance string `json:"instance"`
	URL      string `json:"url"`
	Cluster  string `json:"cluster"`
	Loaded   string `json:"loaded"`
	Started  string `json:"started"`
}

type tierRow struct {
	Cluster      string `json:"cluster"`
	Tier         string `json:"tier"`
	First        string `json:"first"`
	Last         string `json:"last"`
	Objects      int    `json:"objects"`
	Bytes        int64  `json:"bytes"`
	Size         string `json:"size"`
	Measured     string `json:"measured"`
	NewestUpload string `json:"newest_upload"`
	Nodes        string `json:"nodes_last_hour"`
}

func cmdStatus(ctx context.Context, args []string) error {
	return runStatus(ctx, args, os.Stdout, os.Stderr, time.Now())
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) error {
	fs := newFlagSet("status", statusUsage)
	contextFlag := fs.String("context", "", "the context whose bucket the data section lists; default the one in use")
	bucketFlag := fs.String("bucket", "", "the bucket the data section lists (env S3_BUCKET); default the context's")
	cluster := fs.String("cluster", "", "only this cluster's data")
	full := fs.Bool("full", false, "list all of raw, not its last 24 hours of data")
	format := fs.String("format", "tsv", "tsv or json")
	blobsDir := fs.String("blobs-dir", os.Getenv("BLOBS_DIR"), "a local directory laid out like the bucket, for the data section")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *format != "tsv" && *format != "json" {
		return fail(exitUsage, "--format %q: tsv or json", *format)
	}
	var notes []string
	contexts, current, err := statusContexts(*contextFlag)
	if err != nil {
		return err
	}
	instances, err := promInstances()
	if err != nil {
		notes = append(notes, "prometheus instances: "+err.Error())
	}
	var irows []instanceRow
	for _, i := range instances {
		irows = append(irows, instanceRow{i.name, i.URL, i.Cluster, i.Window, i.Started.Format(time.RFC3339)})
	}
	s := &session{plan: map[string][]string{}}
	// a bucket that cannot be read is its error's exit code (3 for a
	// credential), after the rest is shown
	var storeErr error
	switch {
	case *blobsDir != "":
		s.root = *blobsDir
	default:
		st, _, err := storeFor(ctx, *contextFlag, *bucketFlag)
		if err != nil {
			notes = append(notes, "no data section: "+err.Error())
			// with nothing configured there is nothing to fail
			if ee := (*exitError)(nil); !errors.As(err, &ee) || !strings.HasPrefix(ee.msg, "no bucket:") {
				storeErr = err
			}
			break
		}
		s.root, s.store = "s3://"+st.bucket, st
		for i := range contexts {
			if contexts[i].Current {
				contexts[i].Endpoint, contexts[i].Credential = st.endpoint, credentialNote(contexts[i].Profile, st)
			}
		}
	}
	var rows []tierRow
	if s.root != "" {
		var clusters []string
		switch {
		case *cluster != "":
			clusters = []string{*cluster}
		default:
			// a bucket that cannot be listed is a note and the exit code;
			// the other sections are still shown
			if clusters, err = s.clusters(ctx); err != nil {
				notes = append(notes, "no data section: "+err.Error())
				storeErr = err
			}
		}
		for _, c := range clusters {
			r, err := s.clusterStatus(ctx, c, *full, now)
			if err != nil {
				notes = append(notes, fmt.Sprintf("cluster %s: %v", c, err))
				storeErr = err
				continue
			}
			rows = append(rows, r...)
		}
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Contexts   []contextRow  `json:"contexts"`
			Prometheus []instanceRow `json:"prometheus"`
			Data       []tierRow     `json:"data"`
			Notes      []string      `json:"notes"`
		}{contexts, irows, rows, notes})
	} else {
		fmt.Fprintln(stdout, "## contexts")
		fmt.Fprintln(stdout, "context\tcurrent\tbucket\tcluster\tprofile\tendpoint\tcredential")
		for _, r := range contexts {
			mark := ""
			if r.Current {
				mark = "*"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Context, mark, r.Bucket, r.Cluster, r.Profile, r.Endpoint, r.Credential)
		}
		fmt.Fprintln(stdout, "\n## prometheus")
		fmt.Fprintln(stdout, "instance\turl\tcluster\tloaded\tstarted")
		for _, r := range irows {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", r.Instance, r.URL, r.Cluster, r.Loaded, r.Started)
		}
		fmt.Fprintln(stdout, "\n## data")
		fmt.Fprintln(stdout, "cluster\ttier\tfirst\tlast\tobjects\tbytes\tsize\tmeasured\tnewest_upload\tnodes_last_hour")
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n", r.Cluster, r.Tier, r.First, r.Last, r.Objects, r.Bytes, r.Size, r.Measured, r.NewestUpload, r.Nodes)
		}
	}
	for _, n := range notes {
		fmt.Fprintln(stderr, "y-obs: "+n)
	}
	bucket := "none"
	if s.root != "" {
		bucket = strings.TrimPrefix(s.root, "s3://")
	}
	clusters := map[string]bool{}
	for _, r := range rows {
		clusters[r.Cluster] = true
	}
	fmt.Fprintf(stderr, "y-obs: status: %d contexts (in use: %s), %d prometheus instances, %d clusters in bucket %s; %d listing requests\n",
		len(contexts), current, len(irows), len(clusters), bucket, s.listRequests)
	if ee := (*exitError)(nil); errors.As(storeErr, &ee) {
		// the note above says why
		return &exitError{code: ee.code}
	} else if storeErr != nil {
		return &exitError{code: exitStore}
	}
	return nil
}

// statusContexts are the configured contexts and, when S3_BUCKET is set,
// the environment's bucket; and the name of the one in use.
func statusContexts(flagValue string) ([]contextRow, string, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, "", err
	}
	current, explicit := contextChoice(flagValue, cfg)
	var names []string
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	var rows []contextRow
	for _, n := range names {
		cc := cfg.Contexts[n]
		profile := cc.Profile
		if p := os.Getenv("AWS_PROFILE"); n == current && !explicit && p != "" {
			// storeFor: AWS_PROFILE wins over the file's pick
			profile = "AWS_PROFILE " + p
		}
		if profile == "" {
			profile = "(default chain)"
		}
		rows = append(rows, contextRow{Context: n, Current: n == current, Bucket: cc.Bucket, Cluster: cc.Cluster, Profile: profile})
	}
	if current == "" {
		current = "none"
		if b := os.Getenv("S3_BUCKET"); b != "" {
			current = "(environment)"
			rows = append(rows, contextRow{Context: current, Current: true, Bucket: b, Profile: "(default chain)"})
		}
	}
	return rows, current, nil
}

var keyDate = regexp.MustCompile(`/(\d{4}-\d{2}-\d{2})(?:[/.]|$)`)

// clusterStatus is a cluster's row per tier.
func (s *session) clusterStatus(ctx context.Context, cluster string, full bool, now time.Time) ([]tierRow, error) {
	var rows []tierRow
	// raw: the span from its first and last hour, the volume of its last
	// 24 hours of data (or all of it), the nodes of its last hour
	if dates, err := s.dates(ctx, "raw1/"+cluster+"/"); err != nil {
		return nil, err
	} else if len(dates) > 0 {
		first, last := dates[0], dates[len(dates)-1]
		fh, err := s.hourEdge(ctx, "raw1/"+cluster+"/"+first+"/", false)
		if err != nil {
			return nil, err
		}
		lh, err := s.hourEdge(ctx, "raw1/"+cluster+"/"+last+"/", true)
		if err != nil {
			return nil, err
		}
		r := tierRow{Cluster: cluster, Tier: "raw", First: first + "T" + fh, Last: last + "T" + lh}
		lastHour, _ := time.Parse("2006-01-02T15", r.Last)
		var newest time.Time
		nodes := map[string]bool{}
		add := func(o listedObject) {
			r.Objects++
			r.Bytes += o.size
			if o.modified.After(newest) {
				newest = o.modified
			}
			if strings.Contains(o.key, "/"+lastHour.Format("2006-01-02/15")+"/") {
				parts := strings.Split(o.key, "/")
				if len(parts) >= 2 {
					nodes[parts[len(parts)-2]] = true
				}
			}
		}
		if full {
			r.Measured = "all"
			if err := s.listObjects(ctx, "raw1/"+cluster+"/", add); err != nil {
				return nil, err
			}
		} else {
			r.Measured = "24 h to " + r.Last
			for h := lastHour.Add(-23 * time.Hour); !h.After(lastHour); h = h.Add(time.Hour) {
				if err := s.listObjects(ctx, "raw1/"+cluster+"/"+h.Format("2006-01-02/15")+"/", add); err != nil {
					return nil, err
				}
			}
		}
		r.Size = humanBytes(r.Bytes)
		if !newest.IsZero() {
			r.NewestUpload = fmt.Sprintf("%s (%s ago)", newest.UTC().Format(time.RFC3339), now.Sub(newest).Round(time.Second))
		}
		// the cluster agent writes under _k8sagent: an uploader, not a node
		var ns []string
		for n := range nodes {
			if n != k8sAgentNode {
				ns = append(ns, n)
			}
		}
		sort.Strings(ns)
		r.Nodes = fmt.Sprintf("%d (%s)", len(ns), strings.Join(ns, ","))
		if nodes[k8sAgentNode] {
			r.Nodes += " + cluster agent"
		}
		rows = append(rows, r)
	}
	// hourly and daily: listed in full; the span from the keys' dates
	for _, tier := range []string{"hourly", "daily"} {
		r := tierRow{Cluster: cluster, Tier: tier, Measured: "all"}
		var newest time.Time
		err := s.listObjects(ctx, tier+"/"+cluster+"/", func(o listedObject) {
			r.Objects++
			r.Bytes += o.size
			if o.modified.After(newest) {
				newest = o.modified
			}
			if m := keyDate.FindStringSubmatch(o.key); m != nil && !strings.Contains(o.key, "/_tmp/") {
				if r.First == "" || m[1] < r.First {
					r.First = m[1]
				}
				if m[1] > r.Last {
					r.Last = m[1]
				}
			}
		})
		if err != nil {
			return nil, err
		}
		if r.Objects == 0 {
			continue
		}
		r.Size = humanBytes(r.Bytes)
		if !newest.IsZero() {
			r.NewestUpload = fmt.Sprintf("%s (%s ago)", newest.UTC().Format(time.RFC3339), now.Sub(newest).Round(time.Second))
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func humanBytes(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
