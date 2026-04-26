// Package k8sclient wraps the client-go bootstrapping logic.
// Inside a cluster, it uses the pod's mounted ServiceAccount token.
// Outside a cluster (local dev), it falls back to KUBECONFIG.
package k8sclient

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles the typed Kubernetes clientset and the raw REST config.
// The REST config is needed separately by remotecommand.NewSPDYExecutor.
//
// Clientset is typed as kubernetes.Interface (not the concrete *Clientset) so
// unit tests can substitute fake.NewSimpleClientset() without touching the
// REST config. Production code only uses the typed accessors that the
// interface exposes (CoreV1, AppsV1, …).
type Client struct {
	Clientset  kubernetes.Interface
	RestConfig *rest.Config
}

// New returns a Client configured for the current runtime environment.
func New() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback: local kubeconfig for development / integration testing.
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("building kubeconfig: %w", err)
		}
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}

	return &Client{Clientset: cs, RestConfig: cfg}, nil
}
