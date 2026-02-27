package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// NetworkIDs holds the resource IDs for all provisioned networking components.
type NetworkIDs struct {
	VPCID        string
	SubnetID     string
	IGWID        string
	RouteTableID string
}

// CreateNetworking provisions a VPC, public subnet, internet gateway, and route table.
// All resources are tagged with the cluster name for easy identification.
func CreateNetworking(ctx context.Context, client *ec2.Client, clusterName string) (NetworkIDs, error) {
	var ids NetworkIDs

	// --- VPC ---
	vpcOut, err := client.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []types.TagSpecification{
			tag(types.ResourceTypeVpc, clusterName+"-vpc"),
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

	// --- Subnet ---
	subnetOut, err := client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     aws.String(ids.VPCID),
		CidrBlock: aws.String("10.0.1.0/24"),
		TagSpecifications: []types.TagSpecification{
			tag(types.ResourceTypeSubnet, clusterName+"-subnet"),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create subnet: %w", err)
	}
	ids.SubnetID = aws.ToString(subnetOut.Subnet.SubnetId)

	// Auto-assign public IPs so instances get a public IP at launch.
	if _, err = client.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(ids.SubnetID),
		MapPublicIpOnLaunch: &types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return ids, fmt.Errorf("enable public IP on subnet: %w", err)
	}

	// --- Internet Gateway ---
	igwOut, err := client.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []types.TagSpecification{
			tag(types.ResourceTypeInternetGateway, clusterName+"-igw"),
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

	// --- Route Table ---
	rtOut, err := client.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId: aws.String(ids.VPCID),
		TagSpecifications: []types.TagSpecification{
			tag(types.ResourceTypeRouteTable, clusterName+"-rt"),
		},
	})
	if err != nil {
		return ids, fmt.Errorf("create route table: %w", err)
	}
	ids.RouteTableID = aws.ToString(rtOut.RouteTable.RouteTableId)

	if _, err = client.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId:         aws.String(ids.RouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(ids.IGWID),
	}); err != nil {
		return ids, fmt.Errorf("create default route: %w", err)
	}

	if _, err = client.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(ids.RouteTableID),
		SubnetId:     aws.String(ids.SubnetID),
	}); err != nil {
		return ids, fmt.Errorf("associate route table: %w", err)
	}

	return ids, nil
}

// CreateSecurityGroups provisions the control plane and worker security groups.
// Returns (controlPlaneSGID, workerSGID, error).
func CreateSecurityGroups(ctx context.Context, client *ec2.Client, vpcID, clusterName string) (string, string, error) {
	// --- Control plane SG ---
	cpSGOut, err := client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(clusterName + "-cp-sg"),
		Description: aws.String("Talos control plane: k8s API + Talos API"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			tag(types.ResourceTypeSecurityGroup, clusterName+"-cp-sg"),
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
			tag(types.ResourceTypeSecurityGroup, clusterName+"-worker-sg"),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("create worker SG: %w", err)
	}
	wkSGID := aws.ToString(wkSGOut.GroupId)

	// --- Control plane ingress rules ---
	_, err = client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(cpSGID),
		IpPermissions: []types.IpPermission{
			tcpPort(6443, "0.0.0.0/0", "k8s API"),
			tcpPort(50000, "0.0.0.0/0", "Talos API"),
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
			tcpPort(50000, "0.0.0.0/0", "Talos API"),
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
// Instances must already be terminated before calling this.
func DeleteNetworking(ctx context.Context, client *ec2.Client,
	vpcID, subnetID, igwID, routeTableID, cpSGID, wkSGID string) error {

	// Security groups cross-reference each other via UserIdGroupPairs, so AWS
	// won't allow deleting one while the other still has rules referencing it.
	// Revoke all rules from both SGs first, then delete them.
	if cpSGID != "" {
		revokeAllRules(ctx, client, cpSGID)
	}
	if wkSGID != "" {
		revokeAllRules(ctx, client, wkSGID)
	}

	if wkSGID != "" {
		if _, err := client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(wkSGID),
		}); err != nil {
			return fmt.Errorf("delete worker SG: %w", err)
		}
	}
	if cpSGID != "" {
		if _, err := client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(cpSGID),
		}); err != nil {
			return fmt.Errorf("delete CP SG: %w", err)
		}
	}

	// Route table: disassociate then delete.
	if routeTableID != "" {
		rtOut, err := client.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
			RouteTableIds: []string{routeTableID},
		})
		if err == nil && len(rtOut.RouteTables) > 0 {
			for _, assoc := range rtOut.RouteTables[0].Associations {
				if !aws.ToBool(assoc.Main) {
					_, _ = client.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
						AssociationId: assoc.RouteTableAssociationId,
					})
				}
			}
		}
		if _, err := client.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
			RouteTableId: aws.String(routeTableID),
		}); err != nil {
			return fmt.Errorf("delete route table: %w", err)
		}
	}

	// Subnet
	if subnetID != "" {
		if _, err := client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(subnetID),
		}); err != nil {
			return fmt.Errorf("delete subnet: %w", err)
		}
	}

	// Detach and delete IGW
	if igwID != "" && vpcID != "" {
		_, _ = client.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
			VpcId:             aws.String(vpcID),
		})
		if _, err := client.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		}); err != nil {
			return fmt.Errorf("delete internet gateway: %w", err)
		}
	}

	// VPC
	if vpcID != "" {
		if _, err := client.DeleteVpc(ctx, &ec2.DeleteVpcInput{
			VpcId: aws.String(vpcID),
		}); err != nil {
			return fmt.Errorf("delete VPC: %w", err)
		}
	}

	return nil
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

func tag(resourceType types.ResourceType, name string) types.TagSpecification {
	return types.TagSpecification{
		ResourceType: resourceType,
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(name)},
		},
	}
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
