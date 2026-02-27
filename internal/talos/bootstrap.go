package talos

import (
	"context"
	"fmt"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// WaitForTalosAPI polls the Talos API on the control plane until it responds
// to a Version call, indicating the node has booted and loaded its config.
// Polls every 20 seconds for up to 15 minutes.
func WaitForTalosAPI(ctx context.Context, controlPlaneIP string, tc *clientconfig.Config) error {
	const (
		pollInterval = 20 * time.Second
		maxWait      = 15 * time.Minute
	)

	deadline := time.Now().Add(maxWait)
	fmt.Printf("  waiting for Talos API at %s (up to %s)...\n", controlPlaneIP, maxWait)

	for time.Now().Before(deadline) {
		c, err := newTalosClient(ctx, controlPlaneIP, tc)
		if err == nil {
			_, err = c.Version(ctx)
			c.Close()
			if err == nil {
				fmt.Printf("  Talos API is ready at %s\n", controlPlaneIP)
				return nil
			}
		}

		fmt.Printf("  Talos API not yet ready (%v); retrying in %s\n", err, pollInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return fmt.Errorf("Talos API at %s did not become ready within %s", controlPlaneIP, maxWait)
}

// BootstrapCluster sends the Bootstrap RPC to the control plane, which
// initialises etcd and makes the node the first member of the cluster.
// Must only be called once per cluster lifetime.
func BootstrapCluster(ctx context.Context, controlPlaneIP string, tc *clientconfig.Config) error {
	c, err := newTalosClient(ctx, controlPlaneIP, tc)
	if err != nil {
		return fmt.Errorf("create Talos client for bootstrap: %w", err)
	}
	defer c.Close()

	if err := c.Bootstrap(ctx, &machineapi.BootstrapRequest{}); err != nil {
		return fmt.Errorf("bootstrap etcd: %w", err)
	}
	return nil
}

// FetchKubeconfig retrieves the Kubernetes client config from the Talos API
// and returns the raw kubeconfig YAML bytes. The cluster must be fully
// bootstrapped before calling this — the k8s API server needs to be running.
func FetchKubeconfig(ctx context.Context, controlPlaneIP string, tc *clientconfig.Config) ([]byte, error) {
	c, err := newTalosClient(ctx, controlPlaneIP, tc)
	if err != nil {
		return nil, fmt.Errorf("create Talos client: %w", err)
	}
	defer c.Close()

	kc, err := c.Kubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch kubeconfig from Talos API: %w", err)
	}
	return kc, nil
}

// newTalosClient constructs a Talos gRPC client pointed at the given endpoint.
func newTalosClient(ctx context.Context, controlPlaneIP string, tc *clientconfig.Config) (*client.Client, error) {
	c, err := client.New(ctx,
		client.WithConfig(tc),
		client.WithEndpoints(controlPlaneIP),
	)
	if err != nil {
		return nil, fmt.Errorf("new Talos client: %w", err)
	}
	return c, nil
}
