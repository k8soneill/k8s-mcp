package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

// NLBParams configures the Network Load Balancer to be created.
type NLBParams struct {
	Tags            ClusterTags
	VPCID           string
	PublicSubnetID  string
	EIPAllocationID string // cluster EIP is pinned to the NLB
}

// elbTags builds the []elbtypes.Tag slice for ELB resources (NLB, target groups).
// The ELB SDK uses its own Tag type distinct from the EC2 one.
func elbTags(suffix string, ct ClusterTags, extra ...elbtypes.Tag) []elbtypes.Tag {
	tags := []elbtypes.Tag{
		{Key: aws.String("Name"), Value: aws.String(ct.NamePrefix + "-" + suffix)},
		{Key: aws.String("k8s-mcp/cluster-id"), Value: aws.String(ct.ClusterID)},
	}
	return append(tags, extra...)
}

// NLBIDs holds the ARNs of the created NLB resources.
type NLBIDs struct {
	NLBARN              string
	CPTargetGroupARN    string // TCP:6443
	TalosTargetGroupARN string // TCP:50000
}

// CreateNLB creates an internet-facing NLB with the cluster EIP, two target
// groups (k8s API on 6443, Talos API on 50000), and two TCP listeners.
// Waits for the NLB to reach "active" state before returning.
func CreateNLB(ctx context.Context, client *elasticloadbalancingv2.Client, p NLBParams) (NLBIDs, error) {
	var ids NLBIDs

	// NLB/TG names have a 32-char AWS limit. Use "k8s-mcp-<clusterID>-<suffix>"
	// for uniqueness without depending on the cluster name length.
	// The full human-readable name is preserved in the Name tag via elbTags().
	nlbName := "k8s-mcp-" + p.Tags.ClusterID + "-nlb"
	lbOut, err := client.CreateLoadBalancer(ctx, &elasticloadbalancingv2.CreateLoadBalancerInput{
		Name:   aws.String(nlbName),
		Type:   elbtypes.LoadBalancerTypeEnumNetwork,
		Scheme: elbtypes.LoadBalancerSchemeEnumInternetFacing,
		SubnetMappings: []elbtypes.SubnetMapping{
			{
				SubnetId:     aws.String(p.PublicSubnetID),
				AllocationId: aws.String(p.EIPAllocationID),
			},
		},
		Tags: elbTags("nlb", p.Tags),
	})
	if err != nil {
		return ids, fmt.Errorf("create NLB: %w", err)
	}
	if len(lbOut.LoadBalancers) == 0 {
		return ids, fmt.Errorf("no load balancer returned from CreateLoadBalancer")
	}
	ids.NLBARN = aws.ToString(lbOut.LoadBalancers[0].LoadBalancerArn)

	// Wait for the NLB to be active before creating listeners.
	if err = waitForNLBActive(ctx, client, ids.NLBARN); err != nil {
		return ids, fmt.Errorf("wait for NLB active: %w", err)
	}

	// Target group: k8s API (TCP:6443).
	cpTGOut, err := client.CreateTargetGroup(ctx, &elasticloadbalancingv2.CreateTargetGroupInput{
		Name:                aws.String("k8s-mcp-" + p.Tags.ClusterID + "-cp-6443"),
		Protocol:            elbtypes.ProtocolEnumTcp,
		Port:                aws.Int32(6443),
		VpcId:               aws.String(p.VPCID),
		TargetType:          elbtypes.TargetTypeEnumInstance,
		HealthCheckProtocol: elbtypes.ProtocolEnumTcp,
		HealthCheckPort:     aws.String("6443"),
		Tags: elbTags("cp-6443", p.Tags),
	})
	if err != nil {
		return ids, fmt.Errorf("create k8s API target group: %w", err)
	}
	if len(cpTGOut.TargetGroups) == 0 {
		return ids, fmt.Errorf("no target group returned for k8s API")
	}
	ids.CPTargetGroupARN = aws.ToString(cpTGOut.TargetGroups[0].TargetGroupArn)

	// Target group: Talos API (TCP:50000).
	talosTGOut, err := client.CreateTargetGroup(ctx, &elasticloadbalancingv2.CreateTargetGroupInput{
		Name:                aws.String("k8s-mcp-" + p.Tags.ClusterID + "-talos-50000"),
		Protocol:            elbtypes.ProtocolEnumTcp,
		Port:                aws.Int32(50000),
		VpcId:               aws.String(p.VPCID),
		TargetType:          elbtypes.TargetTypeEnumInstance,
		HealthCheckProtocol: elbtypes.ProtocolEnumTcp,
		HealthCheckPort:     aws.String("50000"),
		Tags: elbTags("talos-50000", p.Tags),
	})
	if err != nil {
		return ids, fmt.Errorf("create Talos API target group: %w", err)
	}
	if len(talosTGOut.TargetGroups) == 0 {
		return ids, fmt.Errorf("no target group returned for Talos API")
	}
	ids.TalosTargetGroupARN = aws.ToString(talosTGOut.TargetGroups[0].TargetGroupArn)

	// Listener: TCP:6443 → k8s API target group.
	if _, err = client.CreateListener(ctx, &elasticloadbalancingv2.CreateListenerInput{
		LoadBalancerArn: aws.String(ids.NLBARN),
		Protocol:        elbtypes.ProtocolEnumTcp,
		Port:            aws.Int32(6443),
		DefaultActions: []elbtypes.Action{
			{
				Type:           elbtypes.ActionTypeEnumForward,
				TargetGroupArn: aws.String(ids.CPTargetGroupARN),
			},
		},
	}); err != nil {
		return ids, fmt.Errorf("create k8s API listener: %w", err)
	}

	// Listener: TCP:50000 → Talos API target group.
	if _, err = client.CreateListener(ctx, &elasticloadbalancingv2.CreateListenerInput{
		LoadBalancerArn: aws.String(ids.NLBARN),
		Protocol:        elbtypes.ProtocolEnumTcp,
		Port:            aws.Int32(50000),
		DefaultActions: []elbtypes.Action{
			{
				Type:           elbtypes.ActionTypeEnumForward,
				TargetGroupArn: aws.String(ids.TalosTargetGroupARN),
			},
		},
	}); err != nil {
		return ids, fmt.Errorf("create Talos API listener: %w", err)
	}

	return ids, nil
}

// RegisterTargetsCP registers the control plane instance with both target groups.
func RegisterTargetsCP(ctx context.Context, client *elasticloadbalancingv2.Client,
	cpTGARN, talosTGARN, instanceID string) error {

	target := []elbtypes.TargetDescription{{Id: aws.String(instanceID)}}

	if _, err := client.RegisterTargets(ctx, &elasticloadbalancingv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(cpTGARN),
		Targets:        target,
	}); err != nil {
		return fmt.Errorf("register CP in k8s API target group: %w", err)
	}

	if _, err := client.RegisterTargets(ctx, &elasticloadbalancingv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(talosTGARN),
		Targets:        target,
	}); err != nil {
		return fmt.Errorf("register CP in Talos API target group: %w", err)
	}

	return nil
}

// DeleteNLB deletes the NLB (waiting for full deletion) then deletes both
// target groups. Safe to call with empty strings (skips those resources).
func DeleteNLB(ctx context.Context, client *elasticloadbalancingv2.Client,
	nlbARN, cpTGARN, talosTGARN string) error {

	if nlbARN != "" {
		if _, err := client.DeleteLoadBalancer(ctx, &elasticloadbalancingv2.DeleteLoadBalancerInput{
			LoadBalancerArn: aws.String(nlbARN),
		}); err != nil {
			return fmt.Errorf("delete NLB: %w", err)
		}
		if err := waitForNLBDeleted(ctx, client, nlbARN); err != nil {
			return fmt.Errorf("wait for NLB deletion: %w", err)
		}
	}

	for _, tgARN := range []string{cpTGARN, talosTGARN} {
		if tgARN == "" {
			continue
		}
		if _, err := client.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{
			TargetGroupArn: aws.String(tgARN),
		}); err != nil {
			return fmt.Errorf("delete target group %s: %w", tgARN, err)
		}
	}

	return nil
}

// waitForNLBActive polls until the NLB reaches "active" state (5-minute timeout).
func waitForNLBActive(ctx context.Context, client *elasticloadbalancingv2.Client, arn string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []string{arn},
		})
		if err != nil {
			return fmt.Errorf("describe NLB: %w", err)
		}
		if len(out.LoadBalancers) > 0 &&
			out.LoadBalancers[0].State != nil &&
			out.LoadBalancers[0].State.Code == elbtypes.LoadBalancerStateEnumActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("NLB %s did not reach active state within 5 minutes", arn)
}

// waitForNLBDeleted polls until the NLB is no longer returned (5-minute timeout).
func waitForNLBDeleted(ctx context.Context, client *elasticloadbalancingv2.Client, arn string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []string{arn},
		})
		if err != nil {
			// LoadBalancerNotFoundException means it's fully gone.
			var lbNotFound *elbtypes.LoadBalancerNotFoundException
			if errors.As(err, &lbNotFound) {
				return nil
			}
			return fmt.Errorf("describe NLB: %w", err)
		}
		if len(out.LoadBalancers) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("NLB %s did not reach deleted state within 5 minutes", arn)
}

