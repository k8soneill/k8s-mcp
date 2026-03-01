package cluster

// MaxClusterNameLength is the maximum allowed length for a cluster name.
// AWS Name tags allow 256 chars and SG GroupName allows 255 chars; 63 aligns
// with the Kubernetes label-value and DNS-label limits.
const MaxClusterNameLength = 63

// ClusterConfig holds the desired configuration for a Talos/k8s cluster on AWS.
// Fields are progressively populated during Create() as AWS resources are provisioned.
type ClusterConfig struct {
	// Generated on create — unique per cluster instance so multiple clusters with
	// the same name produce distinct state/config files.
	ClusterID string `json:"cluster_id"`

	// User-supplied
	Name             string       `json:"name"`
	Region           string       `json:"region"`
	TalosVersion     string       `json:"talos_version"`
	KubeVersion      string       `json:"kube_version"`
	ControlPlaneType string       `json:"control_plane_type"`
	WorkerType       string       `json:"worker_type"`
	WorkerCount      int          `json:"worker_count"`
	AllowedCIDRs     AllowedCIDRs `json:"allowed_cidrs"`

	// Populated during Create — networking
	AMIID               string   `json:"ami_id"`
	VPCID               string   `json:"vpc_id"`
	PublicSubnetID      string   `json:"public_subnet_id"`       // 10.0.1.0/24 — NLB + NAT GW
	SubnetID            string   `json:"subnet_id"`              // 10.0.2.0/24 — EC2 instances (private)
	IGWID               string   `json:"igw_id"`
	RouteTableID        string   `json:"route_table_id"`         // public route table → IGW
	PrivateRouteTableID string   `json:"private_route_table_id"` // private route table → NAT GW
	NATGatewayID        string   `json:"nat_gateway_id"`
	NATGatewayEIPID     string   `json:"nat_gateway_eip_id"`
	ControlPlaneSGID    string   `json:"control_plane_sg_id"`
	WorkerSGID          string   `json:"worker_sg_id"`
	// Populated during Create — load balancer
	EIPID               string   `json:"eip_id"`
	ControlPlaneIP      string   `json:"control_plane_ip"`       // NLB Elastic IP
	NLBARN              string   `json:"nlb_arn"`
	CPTargetGroupARN    string   `json:"cp_target_group_arn"`    // TCP:6443
	TalosTargetGroupARN string   `json:"talos_target_group_arn"` // TCP:50000
	// Populated during Create — instances
	ControlPlaneID      string   `json:"control_plane_id"`
	WorkerIDs           []string `json:"worker_ids"`
}

// AllowedCIDRs holds the user-specified source CIDRs for security group ingress rules.
type AllowedCIDRs struct {
	TalosAPI []string `json:"talos_api"` // port 50000
	K8sAPI   []string `json:"k8s_api"`   // port 6443
	Ingress  []string `json:"ingress"`   // ports 80/443 (future)
}

// AllInstanceIDs returns all instance IDs (control plane + workers).
func (c *ClusterConfig) AllInstanceIDs() []string {
	ids := make([]string, 0, 1+len(c.WorkerIDs))
	if c.ControlPlaneID != "" {
		ids = append(ids, c.ControlPlaneID)
	}
	ids = append(ids, c.WorkerIDs...)
	return ids
}

// ClusterState is the full persisted state of a provisioned cluster.
type ClusterState struct {
	Config ClusterConfig `json:"config"`
	Status string        `json:"status"` // creating | running | deleting | deleted
}

// NodeInfo describes a single EC2 node.
type NodeInfo struct {
	InstanceID string
	PublicIP   string
	Role       string // "controlplane" | "worker"
}
