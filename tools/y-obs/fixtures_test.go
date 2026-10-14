package main

// The source cluster's objects for the acceptance test: one of most kinds
// kubectl get is asked about, with the status a kubelet or controller would
// have written, since no controllers run in the test's API server.

const widgetCRD = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  scope: Namespaced
  names: {plural: widgets, singular: widget, kind: Widget, shortNames: [wg]}
  versions:
  # a served version besides the storage version: from the blobs both are
  # served without their schemas (crd_mode redacted)
  - name: v1beta1
    served: true
    storage: false
    additionalPrinterColumns:
    - {name: Size, type: integer, jsonPath: .spec.size}
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            description: the old version
            properties:
              size: {type: integer}
              color: {type: string}
  - name: v1
    served: true
    storage: true
    additionalPrinterColumns:
    - {name: Size, type: integer, jsonPath: .spec.size}
    - {name: Color, type: string, jsonPath: .spec.color, priority: 1}
    - {name: Age, type: date, jsonPath: .metadata.creationTimestamp}
    subresources: {status: {}}
    selectableFields: [{jsonPath: .spec.color}]
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              size: {type: integer}
              color: {type: string}
`

const fixtures = `
apiVersion: v1
kind: Namespace
metadata: {name: app, labels: {team: a}}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: conf, namespace: app}
data: {mode: blue}
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
  namespace: app
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"data":{"password":"aHVudGVyMg=="}}'
type: Opaque
data: {password: aHVudGVyMg==, user: YWRtaW4=}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: app, labels: {app: web}}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: "nginx:1.29"}]
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: app}
spec:
  type: NodePort
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080, nodePort: 30080}]
---
apiVersion: v1
kind: Node
metadata: {name: node1, labels: {kubernetes.io/hostname: node1}}
---
apiVersion: v1
kind: Pod
metadata: {name: web-1, namespace: app, labels: {app: web}}
spec:
  nodeName: node1
  containers: [{name: web, image: "nginx:1.29"}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: migrate, namespace: app}
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{name: m, image: "busybox:1.37"}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web, namespace: app}
spec:
  rules:
  - host: web.example.com
    http:
      paths:
      - {path: /, pathType: Prefix, backend: {service: {name: web, port: {number: 80}}}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: widget-reader}
rules:
- {apiGroups: [example.com], resources: [widgets], verbs: [get, list, watch]}
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: pv1}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  hostPath: {path: /data/pv1}
---
apiVersion: v1
kind: Event
metadata: {name: web-1.started, namespace: app}
involvedObject: {apiVersion: v1, kind: Pod, name: web-1, namespace: app}
reason: Started
message: Started container web
type: Normal
source: {component: kubelet, host: node1}
count: 1
firstTimestamp: "2026-10-01T08:00:00Z"
lastTimestamp: "2026-10-01T08:00:00Z"
---
apiVersion: example.com/v1
kind: Widget
metadata: {name: w1, namespace: app}
spec: {size: 3, color: red}
`

// status writes, as the kubelet and controllers would have
const statuses = `
apiVersion: v1
kind: Node
metadata: {name: node1}
status:
  conditions:
  - {type: Ready, status: "True", reason: KubeletReady, message: kubelet is posting ready status,
     lastHeartbeatTime: "2026-10-01T08:00:00Z", lastTransitionTime: "2026-10-01T08:00:00Z"}
  nodeInfo: {kubeletVersion: v1.37.0, osImage: Ubuntu 24.04, kernelVersion: 6.8.0, containerRuntimeVersion: "containerd://2.1.4",
             architecture: amd64, operatingSystem: linux, bootID: "", machineID: "", systemUUID: "", kubeProxyVersion: ""}
  addresses: [{type: InternalIP, address: 192.168.1.10}]
---
apiVersion: v1
kind: Pod
metadata: {name: web-1, namespace: app}
status:
  phase: Running
  podIP: 10.42.0.7
  podIPs: [{ip: 10.42.0.7}]
  hostIP: 192.168.1.10
  startTime: "2026-10-01T08:00:00Z"
  conditions:
  - {type: Ready, status: "True", lastTransitionTime: "2026-10-01T08:00:01Z"}
  containerStatuses:
  - name: web
    image: "nginx:1.29"
    imageID: "docker.io/library/nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000"
    containerID: "containerd://abc"
    ready: true
    started: true
    restartCount: 0
    state: {running: {startedAt: "2026-10-01T08:00:01Z"}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: app}
status: {replicas: 2, readyReplicas: 1, updatedReplicas: 2, availableReplicas: 1, observedGeneration: 1}
`

// the changes after t1
const changes = `
apiVersion: v1
kind: ConfigMap
metadata: {name: conf, namespace: app}
data: {mode: green}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: later, namespace: app}
data: {a: b}
---
apiVersion: example.com/v1
kind: Widget
metadata: {name: w1, namespace: app}
spec: {size: 5, color: red}
---
apiVersion: v1
kind: Event
metadata: {name: node1.rebooted, namespace: default}
involvedObject: {apiVersion: v1, kind: Node, name: node1, uid: node1}
reason: Rebooted
message: Node node1 has been rebooted
type: Warning
source: {component: kubelet, host: node1}
count: 1
firstTimestamp: "2026-10-01T08:05:00Z"
lastTimestamp: "2026-10-01T08:05:00Z"
`
