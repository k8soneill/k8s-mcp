package cluster

import (
	"context"
	"fmt"
	"log"

	awspkg "k8s-mcp/internal/aws"
	talospkg "k8s-mcp/internal/talos"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// Manager orchestrates cluster create and delete operations.
type Manager struct {
	ec2Client *ec2.Client
	elbClient *elasticloadbalancingv2.Client
	region    string
}

// NewManager creates a Manager for the given AWS region.
func NewManager(ctx context.Context, region string) (*Manager, error) {
	id, err := awspkg.GetCallerIdentity(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("get caller identity: %w", err)
	}
	log.Printf("[aws] running as %s (account %s)", id.ARN, id.Account)

	ec2c, err := awspkg.NewEC2Client(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("init EC2 client: %w", err)
	}
	elbc, err := awspkg.NewELBv2Client(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("init ELBv2 client: %w", err)
	}
	return &Manager{ec2Client: ec2c, elbClient: elbc, region: region}, nil
}

// ProgressFunc is called after each resource allocation step during Create so
// the caller can persist partial state. If the process is interrupted, the
// caller can use the last saved state to run Delete and clean up.
// Errors from ProgressFunc are ignored — persistence failures must not abort provisioning.
type ProgressFunc func(state *ClusterState)

// Create provisions a full Talos/k8s cluster on AWS and returns the cluster
// state and talosconfig. The talosconfig is needed to talk to the Talos API
// and to retrieve a kubeconfig after bootstrapping.
// progress is called after each significant resource is created; pass nil to skip.
// If any step fails, it attempts a best-effort cleanup of already-created resources.
func (m *Manager) Create(ctx context.Context, cfg ClusterConfig, progress ProgressFunc) (*ClusterState, *clientconfig.Config, error) {
	if cfg.ClusterID == "" {
		return nil, nil, fmt.Errorf("ClusterID is required")
	}
	if len(cfg.Name) > MaxClusterNameLength {
		return nil, nil, fmt.Errorf("cluster name %q exceeds %d character limit", cfg.Name, MaxClusterNameLength)
	}

	log.Printf("[create] starting cluster %q (%s) in %s", cfg.Name, cfg.ClusterID, cfg.Region)

	state := &ClusterState{Config: cfg, Status: "creating"}
	var err error

	ct := awspkg.ClusterTags{
		ClusterID:  cfg.ClusterID,
		NamePrefix: cfg.Name + "-" + cfg.ClusterID,
	}

	save := func() {
		if progress != nil {
			progress(state)
		}
	}

	cleanup := func() {
		log.Printf("[create] error during provisioning; attempting partial cleanup")
		if cleanErr := m.Delete(ctx, state); cleanErr != nil {
			log.Printf("[create] cleanup error (resources may be orphaned): %v", cleanErr)
		}
		save() // persist final status
	}

	// 1. Find the Talos AMI.
	if cfg.AMIID == "" {
		log.Printf("[create] looking up official Talos AMI %s for %s", cfg.TalosVersion, cfg.Region)
		cfg.AMIID, err = awspkg.FindTalosAMI(ctx, cfg.Region, cfg.TalosVersion, "amd64")
		if err != nil {
			return nil, nil, fmt.Errorf("find Talos AMI: %w", err)
		}
	}
	state.Config.AMIID = cfg.AMIID
	log.Printf("[create] AMI: %s", state.Config.AMIID)

	// 2. Allocate an Elastic IP for the NLB.
	// Must happen before config generation so the address can be baked into
	// the machine configs. Save immediately — EIP costs start on allocation.
	log.Printf("[create] allocating Elastic IP")
	state.Config.EIPID, state.Config.ControlPlaneIP, err = awspkg.AllocateEIP(ctx, m.ec2Client, cfg.Name, ct)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate EIP: %w", err)
	}
	log.Printf("[create] EIP: %s (%s)", state.Config.EIPID, state.Config.ControlPlaneIP)
	save()

	// 3. Generate Talos machine configs using the EIP as the cluster endpoint.
	log.Printf("[create] generating Talos machine configs")
	configs, err := talospkg.GenerateConfigs(talospkg.GenerateInput{
		ClusterName:          cfg.Name,
		ControlPlaneEndpoint: state.Config.ControlPlaneIP,
		KubeVersion:          cfg.KubeVersion,
		TalosVersion:         cfg.TalosVersion,
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("generate Talos configs: %w", err)
	}

	// 4. Create VPC, public + private subnets, IGW, NAT GW, route tables.
	log.Printf("[create] creating VPC networking")
	netIDs, err := awspkg.CreateNetworking(ctx, m.ec2Client, cfg.Name, ct)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create networking: %w", err)
	}
	state.Config.VPCID = netIDs.VPCID
	state.Config.PublicSubnetID = netIDs.PublicSubnetID
	state.Config.SubnetID = netIDs.PrivateSubnetID // instances go in the private subnet
	state.Config.IGWID = netIDs.IGWID
	state.Config.RouteTableID = netIDs.PublicRouteTableID
	state.Config.PrivateRouteTableID = netIDs.PrivateRouteTableID
	state.Config.NATGatewayID = netIDs.NATGatewayID
	state.Config.NATGatewayEIPID = netIDs.NATGatewayEIPID
	log.Printf("[create] VPC: %s, public subnet: %s, private subnet: %s, NAT GW: %s",
		state.Config.VPCID, state.Config.PublicSubnetID,
		state.Config.SubnetID, state.Config.NATGatewayID)
	save()

	// 5. Create security groups.
	log.Printf("[create] creating security groups")
	state.Config.ControlPlaneSGID, state.Config.WorkerSGID, err = awspkg.CreateSecurityGroups(
		ctx, m.ec2Client, state.Config.VPCID, cfg.Name, ct,
		awspkg.SGAllowedCIDRs{
			TalosAPI: cfg.AllowedCIDRs.TalosAPI,
			K8sAPI:   cfg.AllowedCIDRs.K8sAPI,
			Ingress:  cfg.AllowedCIDRs.Ingress,
		},
	)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create security groups: %w", err)
	}
	save()

	// 6. Create the NLB with the cluster EIP pinned to it.
	// This is what the Talos and k8s configs point to as the control plane endpoint.
	log.Printf("[create] creating NLB with EIP %s", state.Config.EIPID)
	nlbIDs, err := awspkg.CreateNLB(ctx, m.elbClient, awspkg.NLBParams{
		ClusterName:     cfg.Name,
		Tags:            ct,
		VPCID:           state.Config.VPCID,
		PublicSubnetID:  state.Config.PublicSubnetID,
		EIPAllocationID: state.Config.EIPID,
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("create NLB: %w", err)
	}
	state.Config.NLBARN = nlbIDs.NLBARN
	state.Config.CPTargetGroupARN = nlbIDs.CPTargetGroupARN
	state.Config.TalosTargetGroupARN = nlbIDs.TalosTargetGroupARN
	log.Printf("[create] NLB: %s", state.Config.NLBARN)
	save()

	// 7. Launch control plane instance (into the private subnet — no public IP).
	log.Printf("[create] launching control plane instance")
	state.Config.ControlPlaneID, err = awspkg.LaunchInstance(ctx, m.ec2Client, awspkg.LaunchParams{
		ClusterName:  cfg.Name,
		Tags:         ct,
		TalosVersion: cfg.TalosVersion,
		Role:         "controlplane",
		AMIID:        state.Config.AMIID,
		InstanceType: cfg.ControlPlaneType,
		SubnetID:     state.Config.SubnetID,
		SGID:         state.Config.ControlPlaneSGID,
		UserData:     configs.ControlPlane,
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("launch control plane: %w", err)
	}
	log.Printf("[create] control plane instance: %s", state.Config.ControlPlaneID)
	save()

	// 8. Wait for control plane to reach running state.
	log.Printf("[create] waiting for control plane instance to be running")
	if err = awspkg.WaitForInstanceRunning(ctx, m.ec2Client, state.Config.ControlPlaneID); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("wait for control plane: %w", err)
	}

	// 9. Register control plane with both NLB target groups.
	log.Printf("[create] registering control plane with NLB target groups")
	if err = awspkg.RegisterTargetsCP(ctx, m.elbClient,
		state.Config.CPTargetGroupARN,
		state.Config.TalosTargetGroupARN,
		state.Config.ControlPlaneID,
	); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("register NLB targets: %w", err)
	}

	// 10. Launch worker instances (also in the private subnet).
	log.Printf("[create] launching %d worker instance(s)", cfg.WorkerCount)
	state.Config.WorkerIDs = make([]string, 0, cfg.WorkerCount)
	for i := 0; i < cfg.WorkerCount; i++ {
		workerID, err := awspkg.LaunchInstance(ctx, m.ec2Client, awspkg.LaunchParams{
			ClusterName:  cfg.Name,
			Tags:         ct,
			TalosVersion: cfg.TalosVersion,
			Role:         fmt.Sprintf("worker-%d", i),
			AMIID:        state.Config.AMIID,
			InstanceType: cfg.WorkerType,
			SubnetID:     state.Config.SubnetID,
			SGID:         state.Config.WorkerSGID,
			UserData:     configs.Worker,
		})
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("launch worker %d: %w", i, err)
		}
		state.Config.WorkerIDs = append(state.Config.WorkerIDs, workerID)
		log.Printf("[create] worker %d: %s", i, workerID)
		save() // save after each worker so a partial worker list is recoverable
	}

	// 11. Wait for all workers to reach running state.
	log.Printf("[create] waiting for worker instances to be running")
	for _, wid := range state.Config.WorkerIDs {
		if err = awspkg.WaitForInstanceRunning(ctx, m.ec2Client, wid); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("wait for worker %s: %w", wid, err)
		}
	}

	// 12. Wait for Talos API to be available via the NLB EIP.
	log.Printf("[create] waiting for Talos API on %s", state.Config.ControlPlaneIP)
	if err = talospkg.WaitForTalosAPI(ctx, state.Config.ControlPlaneIP, configs.Talosconfig); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("wait for Talos API: %w", err)
	}

	// 13. Bootstrap etcd on the control plane.
	log.Printf("[create] bootstrapping etcd")
	if err = talospkg.BootstrapCluster(ctx, state.Config.ControlPlaneIP, configs.Talosconfig); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("bootstrap cluster: %w", err)
	}

	state.Status = "running"
	save()
	log.Printf("[create] cluster %q is running (control plane: %s)", cfg.Name, state.Config.ControlPlaneIP)
	return state, configs.Talosconfig, nil
}

// Delete tears down all AWS resources for a cluster. It is idempotent: missing
// resources are silently skipped.
func (m *Manager) Delete(ctx context.Context, state *ClusterState) error {
	cfg := state.Config
	log.Printf("[delete] tearing down cluster %q", cfg.Name)
	state.Status = "deleting"

	// 1. Terminate all instances.
	ids := cfg.AllInstanceIDs()
	if len(ids) > 0 {
		log.Printf("[delete] terminating instances: %v", ids)
		if err := awspkg.TerminateInstances(ctx, m.ec2Client, ids); err != nil {
			return fmt.Errorf("terminate instances: %w", err)
		}
	}

	// 2. Delete the NLB and target groups.
	// Must happen before releasing the cluster EIP (NLB holds it) and before
	// deleting networking (NLB sits in the public subnet).
	if cfg.NLBARN != "" || cfg.CPTargetGroupARN != "" || cfg.TalosTargetGroupARN != "" {
		log.Printf("[delete] deleting NLB %s", cfg.NLBARN)
		if err := awspkg.DeleteNLB(ctx, m.elbClient,
			cfg.NLBARN, cfg.CPTargetGroupARN, cfg.TalosTargetGroupARN,
		); err != nil {
			return fmt.Errorf("delete NLB: %w", err)
		}
	}

	// 3. Release the cluster Elastic IP (was held by the NLB).
	if cfg.EIPID != "" {
		log.Printf("[delete] releasing EIP %s", cfg.EIPID)
		if err := awspkg.ReleaseEIP(ctx, m.ec2Client, cfg.EIPID); err != nil {
			log.Printf("[delete] warn: release EIP: %v", err)
		}
	}

	// 4. Delete networking (SGs, NAT GW + its EIP, route tables, subnets, IGW, VPC).
	if cfg.VPCID != "" {
		log.Printf("[delete] deleting networking")
		if err := awspkg.DeleteNetworking(ctx, m.ec2Client, awspkg.DeleteNetworkingParams{
			VPCID:               cfg.VPCID,
			PublicSubnetID:      cfg.PublicSubnetID,
			PrivateSubnetID:     cfg.SubnetID,
			IGWID:               cfg.IGWID,
			PublicRouteTableID:  cfg.RouteTableID,
			PrivateRouteTableID: cfg.PrivateRouteTableID,
			NATGatewayID:        cfg.NATGatewayID,
			NATGatewayEIPID:     cfg.NATGatewayEIPID,
			CPSGID:              cfg.ControlPlaneSGID,
			WorkerSGID:          cfg.WorkerSGID,
		}); err != nil {
			return fmt.Errorf("delete networking: %w", err)
		}
	}

	state.Status = "deleted"
	log.Printf("[delete] cluster %q deleted", cfg.Name)
	return nil
}
