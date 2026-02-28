package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"k8s-mcp/internal/cluster"
	talospkg "k8s-mcp/internal/talos"

	"github.com/spf13/cobra"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cluster",
		Short: "Provision and destroy Talos/k8s clusters on AWS",
	}
	root.AddCommand(createCmd(), deleteCmd(), kubeconfigCmd())
	return root
}

// --- create ---

func createCmd() *cobra.Command {
	var (
		name             string
		region           string
		talosVersion     string
		kubeVersion      string
		workerCount      int
		controlPlaneType string
		workerType       string
		amiID            string
		stateOut         string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new Talos/k8s cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			clusterID := generateClusterID()
			if stateOut == "" {
				stateOut = name + "-" + clusterID + "-state.json"
			}
			talosconfigOut := name + "-" + clusterID + "-talosconfig"

			cfg := cluster.ClusterConfig{
				ClusterID:        clusterID,
				Name:             name,
				Region:           region,
				TalosVersion:     talosVersion,
				KubeVersion:      kubeVersion,
				ControlPlaneType: controlPlaneType,
				WorkerType:       workerType,
				WorkerCount:      workerCount,
				AMIID:            amiID,
			}

			ctx := context.Background()
			mgr, err := cluster.NewManager(ctx, region)
			if err != nil {
				return fmt.Errorf("init manager: %w", err)
			}

			// Write state after each resource allocation so a killed process
			// leaves a file that can be passed to `delete` for cleanup.
			progress := func(s *cluster.ClusterState) {
				if err := writeState(stateOut, s); err != nil {
					log.Printf("warn: could not save state: %v", err)
				}
			}

			state, tc, err := mgr.Create(ctx, cfg, progress)
			if err != nil {
				return fmt.Errorf("create cluster: %w", err)
			}

			// Save talosconfig — needed later to fetch a kubeconfig.
			if err := saveTalosconfig(talosconfigOut, tc); err != nil {
				log.Printf("warn: could not save talosconfig: %v", err)
			}

			fmt.Printf("\nCluster %q created successfully!\n", name)
			fmt.Printf("  Control plane IP : %s\n", state.Config.ControlPlaneIP)
			fmt.Printf("  Workers          : %d\n", len(state.Config.WorkerIDs))
			fmt.Printf("  State saved to   : %s\n", stateOut)
			fmt.Printf("  Talosconfig      : %s\n", talosconfigOut)
			fmt.Printf("\nTo get a kubeconfig once the cluster is ready:\n")
			fmt.Printf("  go run ./cmd/cluster kubeconfig --state %s\n", stateOut)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "cluster name (required)")
	cmd.Flags().StringVar(&region, "region", "us-east-1", "AWS region")
	cmd.Flags().StringVar(&talosVersion, "talos-version", "v1.9.0", "Talos version")
	cmd.Flags().StringVar(&kubeVersion, "kube-version", "v1.32.0", "Kubernetes version")
	cmd.Flags().IntVar(&workerCount, "worker-count", 2, "number of worker nodes")
	cmd.Flags().StringVar(&controlPlaneType, "control-plane-type", "t3.medium", "EC2 instance type for control plane")
	cmd.Flags().StringVar(&workerType, "worker-type", "t3.medium", "EC2 instance type for workers")
	cmd.Flags().StringVar(&amiID, "ami-id", "", "AMI ID to use (skips automatic lookup; required if no official AMI exists for your region/version)")
	cmd.Flags().StringVar(&stateOut, "state-out", "", "path to write cluster state JSON (default: <name>-state.json)")

	return cmd
}

// --- delete ---

func deleteCmd() *cobra.Command {
	var statePath string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a Talos/k8s cluster using a state file",
		RunE: func(cmd *cobra.Command, args []string) error {
			if statePath == "" {
				return fmt.Errorf("--state is required")
			}

			state, err := readState(statePath)
			if err != nil {
				return fmt.Errorf("read state file: %w", err)
			}

			ctx := context.Background()
			mgr, err := cluster.NewManager(ctx, state.Config.Region)
			if err != nil {
				return fmt.Errorf("init manager: %w", err)
			}

			if err := mgr.Delete(ctx, state); err != nil {
				return fmt.Errorf("delete cluster: %w", err)
			}

			// Overwrite state file with final deleted status.
			if err := writeState(statePath, state); err != nil {
				log.Printf("warn: could not update state file: %v", err)
			}

			fmt.Printf("\nCluster %q deleted successfully.\n", state.Config.Name)
			return nil
		},
	}

	cmd.Flags().StringVar(&statePath, "state", "", "path to cluster state JSON file (required)")
	return cmd
}

// --- kubeconfig ---

func kubeconfigCmd() *cobra.Command {
	var (
		statePath       string
		talosconfigPath string
		kubeconfigOut   string
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Fetch the kubeconfig for a running cluster",
		Long: `Connects to the Talos API on the control plane and retrieves the Kubernetes
client config. The cluster must be fully bootstrapped before running this command.
Kubernetes may take a few minutes after bootstrap before it is ready to serve
the kubeconfig — retry if the command fails immediately after create.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if statePath == "" {
				return fmt.Errorf("--state is required")
			}

			state, err := readState(statePath)
			if err != nil {
				return fmt.Errorf("read state file: %w", err)
			}

			// Derive talosconfig path from cluster name + ID if not provided.
			if talosconfigPath == "" {
				if state.Config.ClusterID != "" {
					talosconfigPath = state.Config.Name + "-" + state.Config.ClusterID + "-talosconfig"
				} else {
					talosconfigPath = state.Config.Name + "-talosconfig"
				}
			}
			if kubeconfigOut == "" {
				if state.Config.ClusterID != "" {
					kubeconfigOut = state.Config.Name + "-" + state.Config.ClusterID + "-kubeconfig"
				} else {
					kubeconfigOut = state.Config.Name + "-kubeconfig"
				}
			}

			tc, err := loadTalosconfig(talosconfigPath)
			if err != nil {
				return fmt.Errorf("load talosconfig: %w", err)
			}

			fmt.Printf("Fetching kubeconfig from %s...\n", state.Config.ControlPlaneIP)
			ctx := context.Background()
			kc, err := talospkg.FetchKubeconfig(ctx, state.Config.ControlPlaneIP, tc)
			if err != nil {
				return fmt.Errorf("fetch kubeconfig: %w\n\nNote: Kubernetes may still be initialising. Wait a minute and retry.", err)
			}

			if err := os.WriteFile(kubeconfigOut, kc, 0600); err != nil {
				return fmt.Errorf("write kubeconfig: %w", err)
			}

			fmt.Printf("Kubeconfig written to %s\n", kubeconfigOut)
			fmt.Printf("\nTo use it:\n")
			fmt.Printf("  export KUBECONFIG=$(pwd)/%s\n", kubeconfigOut)
			fmt.Printf("  kubectl get nodes\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&statePath, "state", "", "path to cluster state JSON file (required)")
	cmd.Flags().StringVar(&talosconfigPath, "talosconfig", "", "path to talosconfig file (default: <name>-talosconfig)")
	cmd.Flags().StringVar(&kubeconfigOut, "out", "", "path to write kubeconfig (default: <name>-kubeconfig)")
	return cmd
}

// --- helpers ---

func generateClusterID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeState(path string, state *cluster.ClusterState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

func readState(path string) (*cluster.ClusterState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state cluster.ClusterState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveTalosconfig(path string, tc *clientconfig.Config) error {
	b, err := tc.Bytes()
	if err != nil {
		return fmt.Errorf("marshal talosconfig: %w", err)
	}
	return os.WriteFile(path, b, 0600)
}

func loadTalosconfig(path string) (*clientconfig.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	tc, err := clientconfig.FromBytes(b)
	if err != nil {
		return nil, fmt.Errorf("parse talosconfig: %w", err)
	}
	return tc, nil
}
