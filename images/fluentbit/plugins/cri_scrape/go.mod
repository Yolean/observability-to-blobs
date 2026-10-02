module github.com/Yolean/kubernetes-node-agent-to-blobs/images/fluentbit/plugins/cri_scrape

go 1.26.0

require (
	github.com/fluent/fluent-bit-go v0.0.0-20260616051939-71a89c3094aa
	github.com/prometheus/client_model v0.6.2
	github.com/prometheus/common v0.70.1
	google.golang.org/grpc v1.83.1
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/cri-api v0.36.4
)

require (
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ugorji/go/codec v1.1.7 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af // indirect
)
