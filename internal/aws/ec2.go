package aws

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// LaunchParams configures a single EC2 instance launch.
type LaunchParams struct {
	ClusterName  string
	Tags         ClusterTags
	TalosVersion string
	Role         string // "controlplane" | "worker"
	AMIID        string
	InstanceType string
	SubnetID     string
	SGID         string
	// UserData is the raw Talos machine config YAML.
	// It will be base64-encoded before being sent to the AWS API.
	UserData []byte
}

// AllocateEIP allocates a VPC-domain Elastic IP and returns (allocationID, publicIP).
func AllocateEIP(ctx context.Context, client *ec2.Client, ct ClusterTags) (string, string, error) {
	out, err := client.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain: types.DomainTypeVpc,
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeElasticIp, "eip", ct),
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("allocate EIP: %w", err)
	}
	return aws.ToString(out.AllocationId), aws.ToString(out.PublicIp), nil
}

// AssociateEIP associates an Elastic IP allocation with a running instance.
func AssociateEIP(ctx context.Context, client *ec2.Client, allocationID, instanceID string) error {
	_, err := client.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId: aws.String(allocationID),
		InstanceId:   aws.String(instanceID),
	})
	if err != nil {
		return fmt.Errorf("associate EIP %s with %s: %w", allocationID, instanceID, err)
	}
	return nil
}

// ReleaseEIP releases an Elastic IP allocation. It is idempotent: if the EIP
// is already released it returns nil. If the EIP is still associated with a
// resource (e.g. a recently-deleted NLB), it polls until the association is
// cleared and then releases, retrying for up to 3 minutes.
func ReleaseEIP(ctx context.Context, client *ec2.Client, allocationID string) error {
	const maxAttempts = 12
	for attempt := range maxAttempts {
		_, err := client.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
			AllocationId: aws.String(allocationID),
		})
		if err == nil {
			return nil
		}

		// Already released.
		if isEC2NotFound(err) {
			return nil
		}

		// Check whether the EIP is still associated with a resource.
		// If so, wait and retry (e.g. NLB ENI hasn't been cleaned up yet).
		// If not, this is a real error (permissions, etc.) — fail fast.
		desc, descErr := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
			AllocationIds: []string{allocationID},
		})
		if descErr != nil {
			if isEC2NotFound(descErr) {
				return nil // EIP gone between release and describe
			}
			return fmt.Errorf("release EIP %s: %w", allocationID, err)
		}
		if len(desc.Addresses) == 0 {
			return nil
		}
		if desc.Addresses[0].AssociationId == nil {
			// EIP exists and is not associated — this is a real error.
			return fmt.Errorf("release EIP %s: %w", allocationID, err)
		}

		// EIP is still associated — wait for the association to clear.
		log.Printf("[delete] EIP %s still associated, retrying (%d/%d)", allocationID, attempt+1, maxAttempts)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	return fmt.Errorf("release EIP %s: still associated after %d attempts", allocationID, maxAttempts)
}

// gzipBase64 gzip-compresses data and returns the base64-encoded result.
// EC2 user-data is limited to 25,600 bytes after base64-encoding; Talos machine
// configs exceed this as plain YAML but comfortably fit when compressed.
// EC2 transparently decompresses gzip user-data before passing it to the instance.
func gzipBase64(data []byte) (string, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// LaunchInstance launches a single EC2 instance and returns its instance ID.
// The instance is tagged with its cluster name and role.
func LaunchInstance(ctx context.Context, client *ec2.Client, p LaunchParams) (string, error) {
	// Gzip-compress then base64-encode the machine config. Plain YAML exceeds
	// EC2's 25,600-byte user-data limit; gzip brings it well within range.
	encodedUserData, err := gzipBase64(p.UserData)
	if err != nil {
		return "", fmt.Errorf("compress user data: %w", err)
	}

	out, err := client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String(p.AMIID),
		InstanceType: types.InstanceType(p.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		SubnetId:     aws.String(p.SubnetID),
		SecurityGroupIds: []string{p.SGID},
		UserData:     aws.String(encodedUserData),
		TagSpecifications: []types.TagSpecification{
			clusterResourceTag(types.ResourceTypeInstance, p.Role, p.Tags,
				types.Tag{Key: aws.String("k8s-mcp/role"), Value: aws.String(p.Role)},
				types.Tag{Key: aws.String("k8s-mcp/talos-version"), Value: aws.String(p.TalosVersion)},
			),
		},
		// Disable accidental termination protection isn't set here intentionally —
		// the delete path will handle teardown explicitly.
	})
	if err != nil {
		return "", fmt.Errorf("run instance (%s %s): %w", p.ClusterName, p.Role, err)
	}
	if len(out.Instances) == 0 {
		return "", fmt.Errorf("no instances returned from RunInstances")
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

// WaitForInstanceRunning polls DescribeInstances until the instance reaches
// the "running" state, with a 10-minute timeout.
func WaitForInstanceRunning(ctx context.Context, client *ec2.Client, instanceID string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: []string{instanceID},
		})
		if err != nil {
			return fmt.Errorf("describe instance %s: %w", instanceID, err)
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && inst.State.Name == types.InstanceStateNameRunning {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return fmt.Errorf("instance %s did not reach running state within 10 minutes", instanceID)
}

// TerminateInstances terminates the given instances and waits for them to reach
// the "terminated" state (up to 15 minutes). It is idempotent: instances that
// no longer exist or are already terminated are silently skipped.
func TerminateInstances(ctx context.Context, client *ec2.Client, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	// Pre-filter to instances that still exist and aren't already terminated.
	// Using a filter (not InstanceIds) so DescribeInstances won't error on
	// IDs that have been purged from the API.
	active, err := findActiveInstances(ctx, client, instanceIDs)
	if err != nil {
		return fmt.Errorf("describe instances before termination: %w", err)
	}
	if len(active) == 0 {
		return nil
	}

	if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: active,
	}); err != nil {
		return fmt.Errorf("terminate instances: %w", err)
	}

	// Wait for only the instances we actually terminated.
	deadline := time.Now().Add(15 * time.Minute)
	remaining := make(map[string]struct{}, len(active))
	for _, id := range active {
		remaining[id] = struct{}{}
	}

	for time.Now().Before(deadline) && len(remaining) > 0 {
		// Build a slice from remaining keys to describe only what we're waiting on.
		ids := make([]string, 0, len(remaining))
		for id := range remaining {
			ids = append(ids, id)
		}

		out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			InstanceIds: ids,
		})
		if err != nil {
			if isEC2NotFound(err) {
				return nil
			}
			return fmt.Errorf("describe instances while waiting for termination: %w", err)
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if inst.State != nil && inst.State.Name == types.InstanceStateNameTerminated {
					delete(remaining, aws.ToString(inst.InstanceId))
				}
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}

	if len(remaining) > 0 {
		return fmt.Errorf("instances not terminated within 15 minutes: %v", remaining)
	}
	return nil
}

// findActiveInstances returns the subset of instanceIDs that exist and are not
// yet in the "terminated" state. Uses a filter query so missing IDs don't
// cause an API error.
func findActiveInstances(ctx context.Context, client *ec2.Client, instanceIDs []string) ([]string, error) {
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-id"), Values: instanceIDs},
		},
	})
	if err != nil {
		return nil, err
	}
	var active []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.State != nil && inst.State.Name != types.InstanceStateNameTerminated {
				active = append(active, aws.ToString(inst.InstanceId))
			}
		}
	}
	return active, nil
}
