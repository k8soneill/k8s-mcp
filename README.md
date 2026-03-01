# k8s-mcp

Provisions and destroys vanilla Kubernetes clusters on AWS, using [Talos Linux](https://www.talos.dev/) as the node OS. Topology: one control plane + configurable worker nodes.

This is Phase 1 of a larger project — an MCP server that exposes cluster lifecycle operations as tools to AI agents (Claude Code, etc.).

---

## Architecture

```
cmd/cluster/          ← CLI entrypoint (create / delete)
internal/
  aws/                ← EC2, VPC, EIP, security groups, AMI lookup
  talos/              ← machine config generation, etcd bootstrap
  cluster/            ← orchestration + state types
```

**Create sequence**

1. Look up the official Talos AMI for the requested version + region (from `cloud-images.json`), or use `--ami-id` to provide your own
2. Allocate an Elastic IP (used as the stable cluster endpoint, pinned to the NLB)
3. Generate Talos machine configs — the EIP is embedded as the k8s API endpoint
4. Create VPC with public + private subnets, Internet Gateway, NAT Gateway, and route tables
5. Create security groups — ports 6443 and 50000 are opened to user-configurable CIDRs (default `0.0.0.0/0`) plus the VPC CIDR for NLB health checks
6. Create an internet-facing NLB with the cluster EIP, plus target groups for k8s API (6443) and Talos API (50000)
7. Launch control plane EC2 instance in the private subnet (machine config delivered via user-data)
8. Register control plane with both NLB target groups
9. Launch worker instances in the private subnet
10. Poll the Talos API via the NLB until the control plane is ready (~5–10 min)
11. Bootstrap etcd on the control plane

**Delete sequence**

Reads the state file written by `create` and tears down in reverse order: terminate instances → delete NLB + target groups → release cluster EIP → delete security groups → delete NAT Gateway + its EIP → delete subnets → delete IGW → delete VPC.

---

## Prerequisites

### Tools

| Tool | Version | Install |
|------|---------|---------|
| Go | ≥ 1.23 | https://go.dev/dl |
| AWS CLI | any | `brew install awscli` |

The `talosctl` binary is **not** required — config generation and bootstrapping use the Talos Go SDK directly.

### AWS account setup

#### 1. IAM permissions

The simplest approach for testing is an IAM user or role with the `AmazonEC2FullAccess` managed policy. For a tighter policy, the minimum required actions are:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeAvailabilityZones",
        "ec2:DescribeImages",
        "ec2:CreateVpc",
        "ec2:ModifyVpcAttribute",
        "ec2:DeleteVpc",
        "ec2:CreateSubnet",
        "ec2:ModifySubnetAttribute",
        "ec2:DeleteSubnet",
        "ec2:CreateInternetGateway",
        "ec2:AttachInternetGateway",
        "ec2:DetachInternetGateway",
        "ec2:DeleteInternetGateway",
        "ec2:CreateRouteTable",
        "ec2:CreateRoute",
        "ec2:AssociateRouteTable",
        "ec2:DisassociateRouteTable",
        "ec2:DescribeRouteTables",
        "ec2:DeleteRouteTable",
        "ec2:CreateSecurityGroup",
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:AuthorizeSecurityGroupEgress",
        "ec2:RevokeSecurityGroupIngress",
        "ec2:RevokeSecurityGroupEgress",
        "ec2:DescribeSecurityGroups",
        "ec2:DeleteSecurityGroup",
        "ec2:AllocateAddress",
        "ec2:AssociateAddress",
        "ec2:ReleaseAddress",
        "ec2:DescribeAddresses",
        "ec2:CreateNatGateway",
        "ec2:DeleteNatGateway",
        "ec2:DescribeNatGateways",
        "ec2:RunInstances",
        "ec2:TerminateInstances",
        "ec2:DescribeInstances",
        "ec2:CreateTags",
        "elasticloadbalancing:CreateLoadBalancer",
        "elasticloadbalancing:DeleteLoadBalancer",
        "elasticloadbalancing:DescribeLoadBalancers",
        "elasticloadbalancing:CreateTargetGroup",
        "elasticloadbalancing:DeleteTargetGroup",
        "elasticloadbalancing:DescribeTargetGroups",
        "elasticloadbalancing:CreateListener",
        "elasticloadbalancing:DeleteListener",
        "elasticloadbalancing:DescribeListeners",
        "elasticloadbalancing:RegisterTargets",
        "elasticloadbalancing:DeregisterTargets",
        "elasticloadbalancing:DescribeTargetHealth",
        "elasticloadbalancing:AddTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "sts:GetCallerIdentity",
      "Resource": "*",
      "Sid": "OnlyNeededWithDebugFlag"
    },
    {
      "Effect": "Allow",
      "Action": "iam:CreateServiceLinkedRole",
      "Resource": "*",
      "Condition": {
        "StringLike": {
          "iam:AWSServiceName": "elasticloadbalancing.amazonaws.com"
        }
      }
    }
  ]
}
```

If you are importing a custom Talos AMI (see below), also add `ec2:ImportSnapshot`, `ec2:DescribeImportSnapshotTasks`, `ec2:RegisterImage`, and `s3:PutObject` / `s3:GetObject` on your S3 bucket.

#### 2. Elastic IP quota

Each cluster uses two Elastic IPs (one for the NLB, one for the NAT Gateway). The default AWS quota is 5 EIPs per region. Check your current allocation:

```bash
aws ec2 describe-addresses --query 'Addresses[*].PublicIp'
```

If you are close to the limit, request an increase via **Service Quotas → EC2 → EC2-VPC Elastic IPs**.

#### 3. EC2 instance quota (vCPUs)

`t3.medium` uses 2 vCPUs per instance. A default cluster (1 control plane + 2 workers) requires **6 vCPUs** in the T instance family. Check under **Service Quotas → EC2 → Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances**.

---

## AMI

### Official AMIs (automatic)

Siderolabs publishes official Talos AMIs for each release to 18 AWS regions. The tool fetches the list automatically from the GitHub release artifact:

```
https://github.com/siderolabs/talos/releases/download/<version>/cloud-images.json
```

No manual AMI setup is needed if your region is in the list. Covered regions as of v1.9.x:

`us-east-1`, `us-east-2`, `us-west-1`, `us-west-2`, `ca-central-1`,
`eu-west-1`, `eu-west-2`, `eu-west-3`, `eu-central-1`, `eu-north-1`,
`ap-northeast-1`, `ap-northeast-2`, `ap-northeast-3`, `ap-southeast-1`, `ap-southeast-2`, `ap-south-1`,
`sa-east-1`

If your region is not covered, or you want a custom Talos image (with extensions), import one manually as described below.

### Importing your own AMI

Use this if:
- Your region is not in the official list above
- You want a Talos image with custom extensions (e.g. iSCSI, Nvidia drivers)
- You need a Talos version not yet released to AWS

**Step 1 — Download the Talos disk image**

```bash
TALOS_VERSION=v1.9.5
curl -LO https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/metal-amd64.raw.zst
```

**Step 2 — Decompress**

```bash
zstd --decompress metal-amd64.raw.zst
# produces: metal-amd64.raw
```

**Step 3 — Upload to S3**

```bash
aws s3 mb s3://my-talos-images          # create bucket if it doesn't exist
aws s3 cp metal-amd64.raw s3://my-talos-images/talos-${TALOS_VERSION}-amd64.raw
```

**Step 4 — Import as an EBS snapshot**

```bash
TASK_ID=$(aws ec2 import-snapshot \
  --description "Talos ${TALOS_VERSION} amd64" \
  --disk-container "Format=RAW,UserBucket={S3Bucket=my-talos-images,S3Key=talos-${TALOS_VERSION}-amd64.raw}" \
  --query 'ImportTaskId' --output text)

echo "Import task: ${TASK_ID}"
```

Wait for the import to complete (typically 3–10 minutes):

```bash
aws ec2 describe-import-snapshot-tasks \
  --import-task-ids "${TASK_ID}" \
  --query 'ImportSnapshotTasks[0].SnapshotTaskDetail.[Status,Progress,SnapshotId]' \
  --output table
```

Repeat until `Status` is `completed`. Note the `SnapshotId`.

**Step 5 — Register as an AMI**

```bash
SNAPSHOT_ID=snap-0abc123...   # from step 4

AMI_ID=$(aws ec2 register-image \
  --name "talos-${TALOS_VERSION}-amd64" \
  --description "Talos Linux ${TALOS_VERSION} amd64" \
  --architecture x86_64 \
  --virtualization-type hvm \
  --root-device-name /dev/xvda \
  --block-device-mappings "[{\"DeviceName\":\"/dev/xvda\",\"Ebs\":{\"SnapshotId\":\"${SNAPSHOT_ID}\",\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true}}]" \
  --ena-support \
  --query 'ImageId' --output text)

echo "AMI registered: ${AMI_ID}"
```

**Step 6 — Use it**

Pass the AMI ID with `--ami-id` to skip the automatic lookup:

```bash
go run ./cmd/cluster create \
  --name my-cluster \
  --region ap-southeast-3 \
  --ami-id "${AMI_ID}"
```

---

## Credentials

Set standard AWS environment variables before running any commands:

```bash
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_DEFAULT_REGION=us-east-1   # optional — can be passed as --region flag
```

If you use AWS SSO or named profiles, configure via `AWS_PROFILE` or run `aws sso login` first. The tool uses the standard AWS credential chain, so any method that works with the AWS CLI will work here.

---

## Build

```bash
git clone https://github.com/your-org/k8s-mcp
cd k8s-mcp
go build -o bin/cluster ./cmd/cluster
```

Or run directly without building:

```bash
go run ./cmd/cluster <subcommand> [flags]
```

---

## Usage

### Create a cluster

```bash
go run ./cmd/cluster create \
  --name my-cluster \
  --region us-east-1
```

This creates a cluster named `my-cluster` with the default configuration:
- 1 × `t3.medium` control plane
- 2 × `t3.medium` workers
- Talos v1.12.4 / Kubernetes v1.32.0

State is written to `./my-cluster-<clusterID>-state.json` when provisioning completes.

**Full options:**

```
  --name                  string   cluster name (required)
  --region                string   AWS region (default: us-east-1)
  --talos-version         string   Talos version (default: v1.12.4)
  --kube-version          string   Kubernetes version (default: v1.32.0)
  --worker-count          int      number of worker nodes (default: 2)
  --control-plane-type    string   EC2 instance type for control plane (default: t3.medium)
  --worker-type           string   EC2 instance type for workers (default: t3.medium)
  --ami-id                string   AMI ID to use — skips automatic lookup (optional)
  --state-out             string   path to write state JSON (default: <name>-<clusterID>-state.json)
  --allowed-talos-cidrs   string   allowed source CIDRs for Talos API port 50000 (default: 0.0.0.0/0)
  --allowed-k8s-cidrs     string   allowed source CIDRs for k8s API port 6443 (default: 0.0.0.0/0)
  --allowed-ingress-cidrs string   allowed source CIDRs for ingress 80/443 (default: 0.0.0.0/0)
```

**Example — larger cluster in eu-west-1:**

```bash
go run ./cmd/cluster create \
  --name prod-cluster \
  --region eu-west-1 \
  --worker-count 5 \
  --control-plane-type t3.large \
  --worker-type t3.xlarge \
  --state-out ./prod-cluster-state.json
```

**Example — custom AMI in an unsupported region:**

```bash
go run ./cmd/cluster create \
  --name my-cluster \
  --region me-south-1 \
  --ami-id ami-0abc123...
```

**Example — single-node (control plane only, schedulable):**

```bash
go run ./cmd/cluster create \
  --name dev-cluster \
  --worker-count 0
```

### Delete a cluster

```bash
go run ./cmd/cluster delete --state ./my-cluster-<clusterID>-state.json
```

The state file records all AWS resource IDs created during `create`. Delete uses this to tear down resources in safe order: instances → NLB + target groups → cluster EIP → security groups → NAT Gateway + its EIP → subnets → IGW → VPC.

Deletion is idempotent — resources that no longer exist are silently skipped.

---

## What the state file looks like

`create` writes a JSON file that captures every AWS resource ID. Pass this to `delete` to clean up:

```json
{
  "config": {
    "cluster_id": "3aa4cc10ab",
    "name": "my-cluster",
    "region": "us-east-1",
    "talos_version": "v1.12.4",
    "kube_version": "v1.32.0",
    "control_plane_type": "t3.medium",
    "worker_type": "t3.medium",
    "worker_count": 2,
    "allowed_cidrs": {
      "talos_api": ["0.0.0.0/0"],
      "k8s_api": ["0.0.0.0/0"],
      "ingress": ["0.0.0.0/0"]
    },
    "ami_id": "ami-041649a9ff39ab1cd",
    "vpc_id": "vpc-0abc...",
    "public_subnet_id": "subnet-0pub...",
    "subnet_id": "subnet-0priv...",
    "igw_id": "igw-0abc...",
    "route_table_id": "rtb-0pub...",
    "private_route_table_id": "rtb-0priv...",
    "nat_gateway_id": "nat-0abc...",
    "nat_gateway_eip_id": "eipalloc-0nat...",
    "control_plane_sg_id": "sg-0abc...",
    "worker_sg_id": "sg-0xyz...",
    "eip_id": "eipalloc-0abc...",
    "control_plane_ip": "1.2.3.4",
    "nlb_arn": "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/...",
    "cp_target_group_arn": "arn:aws:elasticloadbalancing:...:targetgroup/...",
    "talos_target_group_arn": "arn:aws:elasticloadbalancing:...:targetgroup/...",
    "control_plane_id": "i-0abc...",
    "worker_ids": ["i-0def...", "i-0ghi..."]
  },
  "status": "running"
}
```

Keep this file safe — it is the only record of which AWS resources belong to each cluster.

---

## Timing

| Phase | Typical duration |
|-------|-----------------|
| AWS infra (VPC, SGs, instances) | ~2 min |
| Talos boot + config apply | ~3–5 min |
| etcd bootstrap | ~30 sec |
| **Total** | **~5–8 min** |

---

## Troubleshooting

**AMI not found for region/version**

Your region may not be in the official Talos AMI list for that version. Either upgrade to the latest Talos version or import a custom AMI and pass `--ami-id`. To see which regions are covered for a given version:

```bash
curl -sL https://github.com/siderolabs/talos/releases/download/v1.9.5/cloud-images.json \
  | python3 -c "import json,sys; [print(i['region']) for i in json.load(sys.stdin) if i['cloud']=='aws' and i['arch']=='amd64']"
```

**Talos API timeout**

If `waiting for Talos API` times out, the instance may have failed to boot. Check the EC2 system log:

```bash
aws ec2 get-console-output \
  --instance-id <control-plane-instance-id> \
  --output text
```

**Partial resources left after a failed create**

`create` attempts cleanup on failure, but if the process was killed mid-run, orphaned resources may remain. All resources are tagged `k8s-mcp/cluster-id=<clusterID>` — search by that tag in the AWS console or CLI to find them:

```bash
aws ec2 describe-instances --filters "Name=tag:k8s-mcp/cluster-id,Values=<clusterID>"
```

**Debugging with talosctl**

If you have `talosctl` installed, you can inspect the control plane node directly using the generated talosconfig:

```bash
# Check service status
talosctl --talosconfig ./<name>-<id>-talosconfig \
  --endpoints <control-plane-ip> --nodes <control-plane-ip> get services

# View service logs (e.g. etcd, kubelet, apid)
talosctl --talosconfig ./<name>-<id>-talosconfig \
  --endpoints <control-plane-ip> --nodes <control-plane-ip> logs etcd

# Check cluster members
talosctl --talosconfig ./<name>-<id>-talosconfig \
  --endpoints <control-plane-ip> --nodes <control-plane-ip> get members

# Inspect machine config applied to the node
talosctl --talosconfig ./<name>-<id>-talosconfig \
  --endpoints <control-plane-ip> --nodes <control-plane-ip> get machineconfig
```

**Delete fails due to dependency order**

If you manually deleted some resources, `delete` may fail on a dependent resource. Re-run `delete` — it skips already-deleted resources and continues.
