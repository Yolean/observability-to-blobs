package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Options configure one served state.
type Options struct {
	// Assets is the directory with the etcd and kube-apiserver binaries,
	// as setup-envtest installs them (KUBEBUILDER_ASSETS)
	Assets string
	// Prepared is the state
	Prepared Prepared
	// Listen is the read-only endpoint's address; 127.0.0.1:0 picks a port
	Listen string
	// ServiceCIDR overrides the service range the state carries
	ServiceCIDR string
	// Label names the state in the read-only endpoint's refusals
	Label string
	// Log gets the API server's and etcd's output; nil discards it
	Log io.Writer
}

// Server is a kube-apiserver over etcd holding one state, behind an HTTP
// endpoint that only lets reads through.
type Server struct {
	etcd  *envtest.Etcd
	api   *envtest.APIServer
	proxy *http.Server
	// URL is the read-only endpoint, plain HTTP on the listen address
	URL string
	// Admin reaches the API server itself, with every permission
	Admin   *rest.Config
	workDir string
}

// Start loads the state into a new etcd and starts the API server on it.
//
// The API server runs no controllers; of its own startup work, what would
// rewrite the state is turned off: the endpoint reconciler (it would point
// default/kubernetes at this process), the priority-and-fairness bootstrap
// (it reconciles the FlowSchemas), and admission webhooks (the services
// they call are not here, and a failing one would block the bootstrap
// writes that remain, e.g. of the system RBAC roles).
func Start(ctx context.Context, o Options) (*Server, error) {
	if o.Listen == "" {
		o.Listen = "127.0.0.1:0"
	}
	out := o.Log
	if out == nil {
		out = io.Discard
	}
	work, err := os.MkdirTemp("", "apiserver-at-")
	if err != nil {
		return nil, err
	}
	s := &Server{workDir: work}
	s.etcd = &envtest.Etcd{
		Path:         filepath.Join(o.Assets, "etcd"),
		DataDir:      filepath.Join(work, "etcd"),
		StartTimeout: time.Minute,
		Out:          out,
		Err:          out,
	}
	if err := s.etcd.Start(); err != nil {
		s.Stop()
		return nil, fmt.Errorf("etcd: %w", err)
	}
	if err := load(ctx, s.etcd.URL.String(), o.Prepared.KVs); err != nil {
		s.Stop()
		return nil, fmt.Errorf("load: %w", err)
	}

	cidr := o.ServiceCIDR
	if cidr == "" {
		cidr = strings.Join(o.Prepared.ServiceCIDRs, ",")
	}
	if cidr == "" {
		cidr = "10.0.0.0/24"
	}
	s.api = &envtest.APIServer{
		Path:         filepath.Join(o.Assets, "kube-apiserver"),
		EtcdURL:      s.etcd.URL,
		CertDir:      filepath.Join(work, "certs"),
		StartTimeout: 2 * time.Minute,
		Out:          out,
		Err:          out,
	}
	if err := os.MkdirAll(s.api.CertDir, 0o700); err != nil {
		s.Stop()
		return nil, err
	}
	args := s.api.Configure()
	args.Set("service-cluster-ip-range", cidr)
	args.Set("storage-media-type", "application/json")
	args.Set("endpoint-reconciler-type", "none")
	args.Set("enable-priority-and-fairness", "false")
	args.Set("disable-admission-plugins", "ServiceAccount,MutatingAdmissionWebhook,ValidatingAdmissionWebhook,ValidatingAdmissionPolicy,MutatingAdmissionPolicy")
	if err := s.api.Start(); err != nil {
		s.Stop()
		return nil, fmt.Errorf("kube-apiserver: %w", err)
	}
	plane := &envtest.ControlPlane{APIServer: s.api, Etcd: s.etcd}
	admin, err := plane.AddUser(envtest.User{Name: "apiserver-at", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.Admin = admin.Config()

	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		s.Stop()
		return nil, err
	}
	h, err := readOnly(s.Admin, o.Label)
	if err != nil {
		ln.Close()
		s.Stop()
		return nil, err
	}
	s.proxy = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.proxy.Serve(ln) }()
	s.URL = "http://" + ln.Addr().String()
	return s, nil
}

// load writes the entries in transactions of etcd's default maximum size.
func load(ctx context.Context, endpoint string, kvs []KV) error {
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 10 * time.Second})
	if err != nil {
		return err
	}
	defer c.Close()
	const batch = 128
	for i := 0; i < len(kvs); i += batch {
		end := min(i+batch, len(kvs))
		ops := make([]clientv3.Op, 0, end-i)
		for _, kv := range kvs[i:end] {
			ops = append(ops, clientv3.OpPut(kv.Key, string(kv.Value)))
		}
		if _, err := c.Txn(ctx).Then(ops...).Commit(); err != nil {
			return err
		}
	}
	return nil
}

// readOnly proxies GET and HEAD to the API server as admin and refuses
// everything else the way the API server refuses: a Status object.
func readOnly(admin *rest.Config, label string) (http.Handler, error) {
	target, err := url.Parse(admin.Host)
	if err != nil {
		return nil, err
	}
	tr, err := rest.TransportFor(admin)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(target)
	p.Transport = tr
	// watches stream
	p.FlushInterval = -1
	msg := "apiserver-at serves the cluster as it was"
	if label != "" {
		msg += " at " + label
	}
	msg += "; it is read-only"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			// the proxy authenticates; a client's own credentials do not pass
			r.Header.Del("Authorization")
			p.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":%q,"reason":"MethodNotAllowed","code":405}`, msg)
	}), nil
}

// KubeConfig is a kubeconfig whose one context reaches the read-only
// endpoint.
func (s *Server) KubeConfig(name string) ([]byte, error) {
	c := clientcmdapi.NewConfig()
	c.Clusters[name] = &clientcmdapi.Cluster{Server: s.URL}
	c.AuthInfos[name] = &clientcmdapi.AuthInfo{}
	c.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	c.CurrentContext = name
	return clientcmd.Write(*c)
}

// Stop stops what Start started and removes its files.
func (s *Server) Stop() {
	if s.proxy != nil {
		_ = s.proxy.Close()
	}
	if s.api != nil {
		_ = s.api.Stop()
	}
	if s.etcd != nil {
		_ = s.etcd.Stop()
	}
	if s.workDir != "" {
		_ = os.RemoveAll(s.workDir)
	}
}
