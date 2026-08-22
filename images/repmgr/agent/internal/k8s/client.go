// Package k8s performs the agent's Kubernetes mutations: pointing the write
// Service at the primary, maintaining pg-role labels for the readonly Service, and
// reading/writing the durable primary-marker ConfigMap. It ports the K8s side of
// the shell service-updater.
package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/cagriekin/pg-ha-agent/internal/kubecfg"
)

// Client wraps a namespaced Kubernetes clientset.
type Client struct {
	cs        kubernetes.Interface
	namespace string
}

// New builds a Client from the resolved apiserver config -- KUBECONFIG when set,
// the in-cluster ServiceAccount otherwise (#317).
func New(namespace string) (*Client, error) {
	rc, err := kubecfg.RESTConfig()
	if err != nil {
		return nil, err
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
