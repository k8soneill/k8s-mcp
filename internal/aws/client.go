package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
)

// NewEC2Client creates an EC2 client for the given region.
// Credentials are loaded from the standard chain:
//   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN env vars,
//   ~/.aws/credentials, IAM instance profile, etc.
func NewEC2Client(ctx context.Context, region string) (*ec2.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return ec2.NewFromConfig(cfg), nil
}

// NewELBv2Client creates an Elastic Load Balancing v2 client for the given region.
func NewELBv2Client(ctx context.Context, region string) (*elasticloadbalancingv2.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return elasticloadbalancingv2.NewFromConfig(cfg), nil
}
