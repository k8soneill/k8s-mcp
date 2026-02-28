// Package talos handles Talos machine config generation and cluster bootstrapping.
package talos

import (
	"fmt"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// GenerateInput holds the parameters needed to generate Talos machine configs.
type GenerateInput struct {
	ClusterName          string
	ControlPlaneEndpoint string // Elastic IP address (no port, no scheme)
	KubeVersion          string // e.g. "v1.32.0"
	TalosVersion         string // e.g. "v1.9.0"
}

// Configs is the output of GenerateConfigs.
type Configs struct {
	ControlPlane []byte               // machine config YAML for the control plane node
	Worker       []byte               // machine config YAML for worker nodes
	Talosconfig  *clientconfig.Config // client credentials to talk to the Talos API
}

// GenerateConfigs creates a Talos secrets bundle and generates machine configs
// for a single control plane and N workers. The control plane endpoint must be
// the Elastic IP address already allocated in AWS (so it can be embedded in the
// generated configs before the instances are launched).
func GenerateConfigs(in GenerateInput) (*Configs, error) {
	// 1. Parse the version contract so cert lifetimes and feature gates
	//    match the requested Talos version.
	vc, err := config.ParseContractFromVersion(in.TalosVersion)
	if err != nil {
		return nil, fmt.Errorf("parse Talos version contract %q: %w", in.TalosVersion, err)
	}

	// 2. Generate a secrets bundle (CA certs, bootstrap tokens, encryption keys).
	secretsBundle, err := secrets.NewBundle(secrets.NewClock(), vc)
	if err != nil {
		return nil, fmt.Errorf("generate secrets bundle: %w", err)
	}

	// 3. Build the generate Input. The endpoint must be a full HTTPS URL with port.
	// The Talos SDK prepends "v" to the kube version when building image tags,
	// so strip any leading "v" from the caller-supplied version string.
	endpoint := "https://" + in.ControlPlaneEndpoint + ":6443"
	input, err := generate.NewInput(
		in.ClusterName,
		endpoint,
		strings.TrimPrefix(in.KubeVersion, "v"),
		generate.WithSecretsBundle(secretsBundle),
		generate.WithVersionContract(vc),
		generate.WithAdditionalSubjectAltNames([]string{in.ControlPlaneEndpoint}),
	)
	if err != nil {
		return nil, fmt.Errorf("build generate input: %w", err)
	}

	// 4. Generate control plane machine config.
	cpProvider, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generate control plane config: %w", err)
	}
	cpBytes, err := cpProvider.EncodeBytes()
	if err != nil {
		return nil, fmt.Errorf("encode control plane config: %w", err)
	}

	// 5. Generate worker machine config.
	wkProvider, err := input.Config(machine.TypeWorker)
	if err != nil {
		return nil, fmt.Errorf("generate worker config: %w", err)
	}
	wkBytes, err := wkProvider.EncodeBytes()
	if err != nil {
		return nil, fmt.Errorf("encode worker config: %w", err)
	}

	// 6. Generate talosconfig (client credentials to talk to the Talos API).
	tc, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("generate talosconfig: %w", err)
	}

	// Embed the control plane IP as the endpoint so the saved talosconfig can
	// be used directly with talosctl without needing an explicit --endpoints flag.
	if ctx, ok := tc.Contexts[tc.Context]; ok {
		ctx.Endpoints = []string{in.ControlPlaneEndpoint}
	}

	return &Configs{
		ControlPlane: cpBytes,
		Worker:       wkBytes,
		Talosconfig:  tc,
	}, nil
}
