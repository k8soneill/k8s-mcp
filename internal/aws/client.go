package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// NewEC2Client creates an EC2 client for the given region.
// Credentials are loaded from the standard chain:
//   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN env vars,
//   ~/.aws/credentials, IAM instance profile, etc.
func NewEC2Client(ctx context.Context, region string) (*ec2.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return ec2.NewFromConfig(cfg), nil
}

// NewELBv2Client creates an Elastic Load Balancing v2 client for the given region.
func NewELBv2Client(ctx context.Context, region string) (*elasticloadbalancingv2.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return elasticloadbalancingv2.NewFromConfig(cfg), nil
}

// CallerIdentity holds the result of sts:GetCallerIdentity.
type CallerIdentity struct {
	Account string
	ARN     string
	UserID  string
}

// GetCallerIdentity calls sts:GetCallerIdentity and returns the account, ARN,
// and user ID of the credentials currently in use.
func GetCallerIdentity(ctx context.Context, region string) (CallerIdentity, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("load AWS config: %w", err)
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("sts:GetCallerIdentity: %w", err)
	}
	return CallerIdentity{
		Account: aws.ToString(out.Account),
		ARN:     aws.ToString(out.Arn),
		UserID:  aws.ToString(out.UserId),
	}, nil
}
