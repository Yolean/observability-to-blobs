// apiserver-at serves a cluster as it was at a time, read-only, to kubectl
// and anything else that speaks the Kubernetes API: a stock kube-apiserver
// over an etcd that holds the objects of local/objects_at.sql and nothing
// else (K8SAGENT.md, "Reading").
//
//	duckdb ... -c "COPY (SELECT * FROM objects_at(TIMESTAMP '...')) TO 'state.jsonl' (FORMAT json)"
//	apiserver-at -objects state.jsonl -kubeconfig at.kubeconfig
//	kubectl --kubeconfig at.kubeconfig get pods -A
//
// local/kubectl-at.sh does both steps against a bucket.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// a FlagSet of its own: controller-runtime registers -kubeconfig on the
	// default one
	fs := flag.NewFlagSet("apiserver-at", flag.ExitOnError)
	objects := fs.String("objects", "-", "the state: JSON lines of objects_at rows, - for stdin")
	assets := fs.String("assets", os.Getenv("KUBEBUILDER_ASSETS"), "directory with etcd and kube-apiserver (setup-envtest use -p path)")
	listen := fs.String("listen", "127.0.0.1:0", "read-only endpoint address")
	kubeconfig := fs.String("kubeconfig", "", "write a kubeconfig for the endpoint here")
	contextName := fs.String("context", "at", "the kubeconfig's context name")
	label := fs.String("label", "", "what the state is, e.g. its time, for messages")
	cidr := fs.String("service-cidr", "", "the cluster's service range; default: from the state's ServiceCIDR")
	verbose := fs.Bool("v", false, "pass through the API server's and etcd's output")
	_ = fs.Parse(os.Args[1:])
	log.SetFlags(0)
	if *assets == "" {
		log.Fatal("-assets or KUBEBUILDER_ASSETS: the directory with etcd and kube-apiserver")
	}

	var in io.Reader = os.Stdin
	if *objects != "-" {
		f, err := os.Open(*objects)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		in = f
	}
	lines, err := ReadLines(in)
	if err != nil {
		log.Fatal(err)
	}
	p, err := Prepare(lines)
	if err != nil {
		log.Fatal(err)
	}
	for reason, n := range p.Skipped {
		log.Printf("left out %d objects: %s", n, reason)
	}
	o := Options{Assets: *assets, Prepared: p, Listen: *listen, ServiceCIDR: *cidr, Label: *label}
	if *verbose {
		o.Log = os.Stderr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := Start(ctx, o)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Stop()
	if *kubeconfig != "" {
		b, err := s.KubeConfig(*contextName)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*kubeconfig, b, 0o600); err != nil {
			log.Fatal(err)
		}
	}
	// the last line before serving, for scripts that wait for it
	fmt.Printf("serving %d objects at %s\n", len(p.KVs), s.URL)
	<-ctx.Done()
}
