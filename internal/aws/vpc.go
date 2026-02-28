package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ClusterTags holds tag values applied to all cluster-owned AWS resources.
// Pass it to every resource-creation function so that tagging is consistent
// and callers don't have to thread individual tag fields through call sites.
type ClusterTags struct {
	ClusterID  string
	NamePrefix string // "<clusterName>-<clusterID>" — used as the prefix for all resource Name tags
}

// NetworkIDs holds the resource IDs for all provisioned networking components.
type NetworkIDs struct {
	VPCID               string
	PublicSubnetID      string // 10.0.1.0/24 — NLB + NAT GW
	PrivateSubnetID     string // 10.0.2.0/24 — EC2 instances
	IGWID               string
	PublicRouteTableID  string // public RT → IGW
	PrivateRouteTableID string // private RT → NAT GW
	NATGatewayID        string
	NATGatewayEIPID     string
}

// DeleteNetworkingParams holds all IDs needed to tear down the network stack.
type DeleteNetworkingParams struct {
	VPCID               string
	PublicSubnetID      string
	PrivateSubnetID     string
	IGWID               string
	PublicRouteTableID  string
	PrivateRouteTableID string
	NATGatewayID        string
	NATGatewayEIPID     string
	CPSGID              string
	WorkerSGID          string
}

// CreateNetworking provisions a two-subnet VPC (public + private) with an
// Internet Gateway, a NAT Gateway (for outbound from private instances), and
// two route tables. All resources are tagged with the cluster name and ID.
func CreateNetworking(ctx context.Context, client *ec2.Client, clusterName string, ct ClusterTags) (NetworkIDs, error) {
	var ids NetworkIDs

	// --- VPC ---
	vpcOut, err := client.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeVpc, "vpc", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create VPC: %w", err)
	}
	ids.VPCID = aws.ToString(vpcOut.Vpc.VpcId)

	// Enable DNS hostnames (required for kubelet node name resolution).
	if _, err = client.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              aws.String(ids.VPCID),
		EnableDnsHostnames: &types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return ids, fmt.Errorf("enable DNS hostnames: %w", err)
	}
	if _, err = client.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:            aws.String(ids.VPCID),
		EnableDnsSupport: &types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return ids, fmt.Errorf("enable DNS support: %w", err)
	}

	// --- Public subnet (NLB + NAT GW — no public IPs auto-assigned) ---
	pubSubOut, err := client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     aws.String(ids.VPCID),
		CidrBlock: aws.String("10.0.1.0/24"),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeSubnet, "public-subnet", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create public subnet: %w", err)
	}
	ids.PublicSubnetID = aws.ToString(pubSubOut.Subnet.SubnetId)

	// --- Private subnet (EC2 instances — no public IPs) ---
	privSubOut, err := client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     aws.String(ids.VPCID),
		CidrBlock: aws.String("10.0.2.0/24"),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeSubnet, "private-subnet", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create private subnet: %w", err)
	}
	ids.PrivateSubnetID = aws.ToString(privSubOut.Subnet.SubnetId)

	// --- Internet Gateway ---
	igwOut, err := client.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeInternetGateway, "igw", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create internet gateway: %w", err)
	}
	ids.IGWID = aws.ToString(igwOut.InternetGateway.InternetGatewayId)

	if _, err = client.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(ids.IGWID),
		VpcId:             aws.String(ids.VPCID),
	}); err != nil {
		return ids, fmt.Errorf("attach internet gateway: %w", err)
	}

	// --- Public route table → IGW ---
	pubRTOut, err := client.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(ids.VPCID),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeRouteTable, "public-rt", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create public route table: %w", err)
	}
	ids.PublicRouteTableID = aws.ToString(pubRTOut.RouteTable.RouteTableId)

	if _, err = client.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(ids.PublicRouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(ids.IGWID),
	}); err != nil {
		return ids, fmt.Errorf("create public default route: %w", err)
	}

	if _, err = client.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(ids.PublicRouteTableID),
		SubnetId:     aws.String(ids.PublicSubnetID),
	}); err != nil {
		return ids, fmt.Errorf("associate public route table: %w", err)
	}

	// --- NAT Gateway EIP (separate from the cluster EIP on the NLB) ---
	natEIPOut, err := client.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: types.DomainTypeVpc,
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeElasticIp, "nat-eip", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("allocate NAT EIP: %w", err)
	}
	ids.NATGatewayEIPID = aws.ToString(natEIPOut.AllocationId)

	// --- NAT Gateway (in public subnet) ---
	natOut, err := client.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId:         aws.String(ids.PublicSubnetID),
		AllocationId:     aws.String(ids.NATGatewayEIPID),
		ConnectivityType: types.ConnectivityTypePublic,
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeNatgateway, "nat", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create NAT gateway: %w", err)
	}
	ids.NATGatewayID = aws.ToString(natOut.NatGateway.NatGatewayId)

	// Wait for NAT Gateway to be available before routing private traffic through it.
	if err = waitForNATGatewayAvailable(ctx, client, ids.NATGatewayID); err != nil {
		return ids, fmt.Errorf("wait for NAT gateway: %w", err)
	}

	// --- Private route table → NAT GW ---
	privRTOut, err := client.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(ids.VPCID),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeRouteTable, "private-rt", ct),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create private route table: %w", err)
	}
	ids.PrivateRouteTableID = aws.ToString(privRTOut.RouteTable.RouteTableId)

	if _, err = client.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(ids.PrivateRouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		NatGatewayId:         aws.String(ids.NATGatewayID),
	}); err != nil {
		return ids, fmt.Errorf("create private default route: %w", err)
	}

	if _, err = client.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(ids.PrivateRouteTableID),
		SubnetId:     aws.String(ids.PrivateSubnetID),
	}); err != nil {
		return ids, fmt.Errorf("associate private route table: %w", err)
	}

	return ids, nil
}

// waitForNATGatewayAvailable polls until the NAT gateway reaches "available" state.
func waitForNATGatewayAvailable(ctx context.Context, client *ec2.Client, natGWID string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natGWID},
		})
		if err != nil {
			return fmt.Errorf("describe NAT gateway: %w", err)
		}
		if len(out.NatGateways) > 0 && out.NatGateways[0].State == types.NatGatewayStateAvailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("NAT gateway %s did not reach available state within 3 minutes", natGWID)
}

// WaitForNATGatewayDeleted polls until the NAT gateway reaches "deleted" state.
func WaitForNATGatewayDeleted(ctx context.Context, client *ec2.Client, natGWID string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
			NatGatewayIds: []string{natGWID},
		})
		if err != nil {
			return fmt.Errorf("describe NAT gateway: %w", err)
		}
		if len(out.NatGateways) == 0 || out.NatGateways[0].State == types.NatGatewayStateDeleted {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("NAT gateway %s did not reach deleted state within 5 minutes", natGWID)
}

// CreateSecurityGroups provisions the control plane and worker security groups.
// Ports 6443 and 50000 are restricted to the VPC CIDR — NLB health checks
// originate from within the VPC so this is sufficient while blocking direct
// internet access to the private instances.
// Returns (controlPlaneSGID, workerSGID, error).
func CreateSecurityGroups(ctx context.Context, client *ec2.Client, vpcID, clusterName string, ct ClusterTags) (string, string, error) {
	const vpcCIDR = "10.0.0.0/16"

	// --- Control plane SG ---
	cpSGOut, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(clusterName + "-cp-sg"),
		Description: aws.String("Talos control plane: k8s API + Talos API"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeSecurityGroup, "cp-sg", ct),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create control plane SG: %w", err)
	}
	cpSGID := aws.ToString(cpSGOut.GroupId)

	// --- Worker SG ---
	wkSGOut, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(clusterName + "-worker-sg"),
		Description: aws.String("Talos workers"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeSecurityGroup, "worker-sg", ct),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create worker SG: %w", err)
	}
	wkSGID := aws.ToString(wkSGOut.GroupId)

	// --- Control plane ingress rules ---
	// TCP 6443 and 50000 allow from VPC CIDR only (NLB health checks + internal traffic).
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(cpSGID),
		IpPermissions: []types.IpPermission{
			tcpPort(6443, vpcCIDR, "k8s API via NLB"),
			tcpPort(50000, vpcCIDR, "Talos API via NLB"),
			tcpPortSG(2379, 2380, cpSGID, "etcd"),
			allTrafficSG(cpSGID, "intra-CP"),
			allTrafficSG(wkSGID, "workers-to-CP"),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("authorize CP ingress: %w", err)
	}

	// --- Worker ingress rules ---
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(wkSGID),
		IpPermissions: []types.IpPermission{
			tcpPort(50000, vpcCIDR, "Talos API via NLB"),
			allTrafficSG(wkSGID, "intra-worker"),
			allTrafficSG(cpSGID, "CP-to-workers"),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("authorize worker ingress: %w", err)
	}

	return cpSGID, wkSGID, nil
}

// DeleteNetworking deletes all networking resources for a cluster.
// The NLB must be deleted and instances terminated before calling this.
func DeleteNetworking(ctx context.Context, client *ec2.Client, p DeleteNetworkingParams) error {
	// Security groups cross-reference each other via UserIdGroupPairs, so
	// revoke all rules from both before deleting either.
	if p.CPSGID != "" {
		revokeAllRules(ctx, client, p.CPSGID)
	}
	if p.WorkerSGID != "" {
		revokeAllRules(ctx, client, p.WorkerSGID)
	}

	if p.WorkerSGID != "" {
		if _, err := client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(p.WorkerSGID),
		}); err != nil {
			return fmt.Errorf("delete worker SG: %w", err)
		}
	}
	if p.CPSGID != "" {
		if _, err := client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(p.CPSGID),
		}); err != nil {
			return fmt.Errorf("delete CP SG: %w", err)
		}
	}

	// Disassociate and delete private route table.
	if p.PrivateRouteTableID != "" {
		disassociateRouteTable(ctx, client, p.PrivateRouteTableID)
		if _, err := client.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(p.PrivateRouteTableID),
		}); err != nil {
			return fmt.Errorf("delete private route table: %w", err)
		}
	}

	// Disassociate and delete public route table.
	if p.PublicRouteTableID != "" {
		disassociateRouteTable(ctx, client, p.PublicRouteTableID)
		if _, err := client.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(p.PublicRouteTableID),
		}); err != nil {
			return fmt.Errorf("delete public route table: %w", err)
		}
	}

	// Delete NAT Gateway (async) and wait for it to be fully gone before
	// releasing the EIP — AWS won't release an EIP still in use by a NAT GW.
	if p.NATGatewayID != "" {
		if _, err := client.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{
			NatGatewayId: aws.String(p.NATGatewayID),
		}); err != nil {
			return fmt.Errorf("delete NAT gateway: %w", err)
		}
		if err := WaitForNATGatewayDeleted(ctx, client, p.NATGatewayID); err != nil {
			return fmt.Errorf("wait for NAT gateway deletion: %w", err)
		}
	}

	if p.NATGatewayEIPID != "" {
		if _, err := client.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
			AllocationId: aws.String(p.NATGatewayEIPID),
		}); err != nil {
			// Log but continue — failure here won't block VPC deletion.
			fmt.Printf("[delete] warn: release NAT EIP %s: %v\n", p.NATGatewayEIPID, err)
		}
	}

	// Delete private subnet.
	if p.PrivateSubnetID != "" {
		if _, err := client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(p.PrivateSubnetID),
		}); err != nil {
			return fmt.Errorf("delete private subnet: %w", err)
		}
	}

	// Delete public subnet.
	if p.PublicSubnetID != "" {
		if _, err := client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(p.PublicSubnetID),
		}); err != nil {
			return fmt.Errorf("delete public subnet: %w", err)
		}
	}

	// Detach and delete IGW.
	if p.IGWID != "" && p.VPCID != "" {
		_, _ = client.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(p.IGWID),
			VpcId:             aws.String(p.VPCID),
		})
		if _, err := client.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(p.IGWID),
		}); err != nil {
			return fmt.Errorf("delete internet gateway: %w", err)
		}
	}

	// VPC.
	if p.VPCID != "" {
		if _, err := client.DeleteVpc(ctx, &ec2.DeleteVpcInput{
			VpcId: aws.String(p.VPCID),
		}); err != nil {
			return fmt.Errorf("delete VPC: %w", err)
		}
	}

	return nil
}

// disassociateRouteTable fetches all non-main associations for a route table
// and disassociates them before deletion.
func disassociateRouteTable(ctx context.Context, client *ec2.Client, routeTableID string) {
	out, err := client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		RouteTableIds: []string{routeTableID},
	})
	if err != nil || len(out.RouteTables) == 0 {
		return
	}
	for _, assoc := range out.RouteTables[0].Associations {
		if !aws.ToBool(assoc.Main) {
			_, _ = client.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
				AssociationId: assoc.RouteTableAssociationId,
			})
		}
	}
}

// revokeAllRules removes every ingress and egress rule from a security group.
// This is required before deletion when SGs reference each other in rules —
// AWS won't delete a SG that is still referenced by another SG's rules.
// Errors are silently ignored since this is best-effort pre-deletion cleanup.
func revokeAllRules(ctx context.Context, client *ec2.Client, sgID string) {
	out, err := client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		GroupIds: []string{sgID},
	})
	if err != nil || len(out.SecurityGroups) == 0 {
		return
	}
	sg := out.SecurityGroups[0]

	if len(sg.IpPermissions) > 0 {
		_, _ = client.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: sg.IpPermissions,
		})
	}
	if len(sg.IpPermissionsEgress) > 0 {
		_, _ = client.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: sg.IpPermissionsEgress,
		})
	}
}

// --- helpers ---

// clusterResourceTag returns a TagSpecification with Name and k8s-mcp/cluster-id
// tags plus any resource-specific extras. The suffix (e.g. "-vpc") is appended
// to ct.NamePrefix to form the full Name tag value.
func clusterResourceTag(resourceType types.ResourceType, suffix string, ct ClusterTags, extra ...types.Tag) types.TagSpecification {
	tags := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String(ct.NamePrefix + "-" + suffix)},
		{Key: aws.String("k8s-mcp/cluster-id"), Value: aws.String(ct.ClusterID)},
	}
	return types.TagSpecification{ResourceType: resourceType, Tags: append(tags, extra...)}
}

func tcpPort(port int32, cidr, description string) types.IpPermission {
	return types.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int32(port),
		ToPort:     aws.Int32(port),
		IpRanges: []types.IpRange{
			{CidrIp: aws.String(cidr), Description: aws.String(description)},
		},
	}
}

func tcpPortSG(from, to int32, sgID, description string) types.IpPermission {
	return types.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int32(from),
		ToPort:     aws.Int32(to),
		UserIdGroupPairs: []types.UserIdGroupPair{
			{GroupId: aws.String(sgID), Description: aws.String(description)},
		},
	}
}

func allTrafficSG(sgID, description string) types.IpPermission {
	return types.IpPermission{
		IpProtocol: aws.String("-1"), // all protocols
		FromPort:   aws.Int32(-1),
		ToPort:     aws.Int32(-1),
		UserIdGroupPairs: []types.UserIdGroupPair{
			{GroupId: aws.String(sgID), Description: aws.String(description)},
		},
	}
}
