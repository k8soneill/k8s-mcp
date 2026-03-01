package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"

	"k8s-mcp/internal/cluster"
	talospkg "k8s-mcp/internal/talos"

	"github.com/spf13/cobra"
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
	root.PersistentFlags().Bool("debug", false, "enable debug logging (e.g. print AWS caller identity)")
	root.AddCommand(createCmd(), deleteCmd(), kubeconfigCmd())
	return root
}

// --- create ---

func createCmd() *cobra.Command {
	var (
		name                string
		region              string
		talosVersion        string
		kubeVersion         string
		workerCount         int
		controlPlaneType    string
		workerType          string
		amiID               string
		stateOut            string
		allowedTalosCIDRs   string
		allowedK8sCIDRs     string
		allowedIngressCIDRs string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new Talos/k8s cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if len(name) > cluster.MaxClusterNameLength {
				return fmt.Errorf("--name %q exceeds %d character limit", name, cluster.MaxClusterNameLength)
			}
			clusterID, err := generateClusterID()
			if err != nil {
				return err
			}
			if stateOut == "" {
				stateOut = name + "-" + clusterID + "-state.json"
			}
			talosconfigOut := name + "-" + clusterID + "-talosconfig"

			talosCIDRs, err := parseCIDRs(allowedTalosCIDRs)
			if err != nil {
				return fmt.Errorf("--allowed-talos-cidrs: %w", err)
			}
			k8sCIDRs, err := parseCIDRs(allowedK8sCIDRs)
			if err != nil {
				return fmt.Errorf("--allowed-k8s-cidrs: %w", err)
			}
			ingressCIDRs, err := parseCIDRs(allowedIngressCIDRs)
			if err != nil {
				return fmt.Errorf("--allowed-ingress-cidrs: %w", err)
			}
			allowedCIDRs := cluster.AllowedCIDRs{
				TalosAPI: talosCIDRs,
				K8sAPI:   k8sCIDRs,
				Ingress:  ingressCIDRs,
			}
			warnOpenCIDRs("--allowed-talos-cidrs", allowedCIDRs.TalosAPI)
			warnOpenCIDRs("--allowed-k8s-cidrs", allowedCIDRs.K8sAPI)
			warnOpenCIDRs("--allowed-ingress-cidrs", allowedCIDRs.Ingress)

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
				AllowedCIDRs:     allowedCIDRs,
			}

			// Setup manager this returns a Manager struct with ec2 and elb clients and the region
			ctx := context.Background()
			debug, _ := cmd.Flags().GetBool("debug")
			mgr, err := cluster.NewManager(ctx, region, debug)
			if err != nil {
				return fmt.Errorf("init manager: %w", err)
			}

			// Write state after each resource allocation so a killed process
			// leaves a file that can be passed to `delete` for cleanup.
			progress := func(s *cluster.ClusterState) {
				if err := cluster.WriteState(stateOut, s); err != nil {
					log.Printf("warn: could not save state: %v", err)
				}
			}

			state, tc, err := mgr.Create(ctx, cfg, progress)
			if err != nil {
				return fmt.Errorf("create cluster: %w", err)
			}

			// Save talosconfig — needed later to fetch a kubeconfig.
			if err := talospkg.SaveTalosconfig(talosconfigOut, tc); err != nil {
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
	cmd.Flags().StringVar(&talosVersion, "talos-version", "v1.12.4", "Talos version")
	cmd.Flags().StringVar(&kubeVersion, "kube-version", "v1.32.0", "Kubernetes version")
	cmd.Flags().IntVar(&workerCount, "worker-count", 2, "number of worker nodes")
	cmd.Flags().StringVar(&controlPlaneType, "control-plane-type", "t3.medium", "EC2 instance type for control plane")
	cmd.Flags().StringVar(&workerType, "worker-type", "t3.medium", "EC2 instance type for workers")
	cmd.Flags().StringVar(&amiID, "ami-id", "", "AMI ID to use (skips automatic lookup; required if no official AMI exists for your region/version)")
	cmd.Flags().StringVar(&stateOut, "state-out", "", "path to write cluster state JSON (default: <name>-<clusterID>-state.json)")
	cmd.Flags().StringVar(&allowedTalosCIDRs, "allowed-talos-cidrs", "0.0.0.0/0", "allowed source CIDRs for Talos API (comma-separated)")
	cmd.Flags().StringVar(&allowedK8sCIDRs, "allowed-k8s-cidrs", "0.0.0.0/0", "allowed source CIDRs for k8s API (comma-separated)")
	cmd.Flags().StringVar(&allowedIngressCIDRs, "allowed-ingress-cidrs", "0.0.0.0/0", "allowed source CIDRs for ingress 80/443 (comma-separated)")

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

			state, err := cluster.ReadState(statePath)
			if err != nil {
				return err
			}

			ctx := context.Background()
			debug, _ := cmd.Flags().GetBool("debug")
			mgr, err := cluster.NewManager(ctx, state.Config.Region, debug)
			if err != nil {
				return fmt.Errorf("init manager: %w", err)
			}

			if err := mgr.Delete(ctx, state); err != nil {
				return fmt.Errorf("delete cluster: %w", err)
			}

			// Overwrite state file with final deleted status.
			if err := cluster.WriteState(statePath, state); err != nil {
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

			state, err := cluster.ReadState(statePath)
			if err != nil {
				return err
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

			tc, err := talospkg.LoadTalosconfig(talosconfigPath)
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

func parseCIDRs(s string) ([]string, error) {
	var cidrs []string
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := netip.ParsePrefix(c); err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		cidrs = append(cidrs, c)
	}
	return cidrs, nil
}

func warnOpenCIDRs(flag string, cidrs []string) {
	for _, c := range cidrs {
		if c == "0.0.0.0/0" {
			log.Printf("WARNING: %s is open to 0.0.0.0/0 -- consider restricting to specific IPs", flag)
			return
		}
	}
}

func generateClusterID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate cluster ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
