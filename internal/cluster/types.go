package cluster

// ClusterConfig holds the desired configuration for a Talos/k8s cluster on AWS.
// Fields are progressively populated during Create() as AWS resources are provisioned.
type ClusterConfig struct {
	// User-supplied
	Name             string `json:"name"`
	Region           string `json:"region"`
	TalosVersion     string `json:"talos_version"`
	KubeVersion      string `json:"kube_version"`
	ControlPlaneType string `json:"control_plane_type"`
	WorkerType       string `json:"worker_type"`
	WorkerCount      int    `json:"worker_count"`

	// Populated during Create
	AMIID            string   `json:"ami_id"`
	VPCID            string   `json:"vpc_id"`
	SubnetID         string   `json:"subnet_id"`
	IGWID            string   `json:"igw_id"`
	RouteTableID     string   `json:"route_table_id"`
	ControlPlaneSGID string   `json:"control_plane_sg_id"`
	WorkerSGID       string   `json:"worker_sg_id"`
	EIPID            string   `json:"eip_id"`
	ControlPlaneIP   string   `json:"control_plane_ip"`
	ControlPlaneID   string   `json:"control_plane_id"`
	WorkerIDs        []string `json:"worker_ids"`
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
