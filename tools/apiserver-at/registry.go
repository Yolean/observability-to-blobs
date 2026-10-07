package main

import "k8s.io/apimachinery/pkg/runtime/schema"

// The API server's etcd key layout, under its default --etcd-prefix:
//
//	/registry/{prefix}/{namespace}/{name}   namespaced
//	/registry/{prefix}/{name}               cluster-scoped
//
// The prefix of a built-in resource is its plural name, without the group,
// except for the resources that kept the names they had before groups
// existed (kube-apiserver's SpecialDefaultResourcePrefixes). Custom
// resources and the two resources of the servers that kube-apiserver
// embeds, CRDs and APIServices, are prefixed with their group. k3s's kine
// keeps the same keys.
const etcdPrefix = "/registry"

var specialPrefixes = map[schema.GroupResource]string{
	{Resource: "replicationcontrollers"}:                "controllers",
	{Resource: "endpoints"}:                             "services/endpoints",
	{Resource: "nodes"}:                                 "minions",
	{Resource: "services"}:                              "services/specs",
	{Group: "networking.k8s.io", Resource: "ingresses"}: "ingress",
	{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}: "apiextensions.k8s.io/customresourcedefinitions",
	{Group: "apiregistration.k8s.io", Resource: "apiservices"}:             "apiregistration.k8s.io/apiservices",
}

// Key is the etcd key of an object; custom tells that a CRD defines its
// resource.
func Key(gr schema.GroupResource, custom bool, namespace, name string) string {
	prefix, ok := specialPrefixes[gr]
	if !ok {
		prefix = gr.Resource
		if custom {
			prefix = gr.Group + "/" + gr.Resource
		}
	}
	k := etcdPrefix + "/" + prefix + "/"
	if namespace != "" {
		k += namespace + "/"
	}
	return k + name
}
