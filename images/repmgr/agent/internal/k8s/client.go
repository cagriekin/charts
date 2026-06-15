// Package k8s performs the agent's Kubernetes mutations: pointing the write
// Service at the primary, maintaining pg-role labels for the readonly Service, and
// reading/writing the durable primary-marker ConfigMap. It ports the K8s side of
// the shell service-updater.
package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Client wraps a namespaced Kubernetes clientset.
type Client struct {
	cs        kubernetes.Interface
	namespace string
}

// New builds a Client from the in-cluster config and ServiceAccount.
func New(namespace string) (*Client, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &Client{cs: cs, namespace: namespace}, nil
}

// NewWithClient builds a Client with an injected clientset (for tests).
func NewWithClient(cs kubernetes.Interface, namespace string) *Client {
	return &Client{cs: cs, namespace: namespace}
}
