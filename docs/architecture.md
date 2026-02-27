# Architecture

## AWS Infrastructure

Each cluster is provisioned inside a dedicated VPC with a two-tier network: a public subnet for internet-facing infrastructure (NLB, NAT Gateway) and a private subnet for EC2 instances. No instance has a public IP address.

```mermaid
flowchart TB
    Internet(["Internet"])

    subgraph vpc["AWS VPC — 10.0.0.0/16"]
        IGW["Internet Gateway"]

        subgraph pub["Public Subnet — 10.0.1.0/24"]
            NLB["Network Load Balancer\n━━━━━━━━━━━━━━━━━━━━━━━━\nElastic IP  =  ControlPlaneIP\n\nListener TCP 6443 → k8s API TG\nListener TCP 50000 → Talos API TG"]
            NAT["NAT Gateway\n(own Elastic IP)"]
        end

        subgraph priv["Private Subnet — 10.0.2.0/24  ·  no public IPs"]
            CP["Control Plane\nEC2 (default: t3.medium)"]
            W["Workers × N\nEC2 (default: t3.medium)"]
        end
    end

    Internet <-->|"internet"| IGW
    IGW -->|"inbound TCP 6443 / 50000"| NLB
    NLB -->|"TCP 6443"| CP
    NLB -->|"TCP 50000"| CP
    CP & W -->|"outbound\n(image pulls, DNS, etc.)"| NAT
    NAT --> IGW
```

**Key design decisions:**

- The cluster's Elastic IP is pinned to the NLB at creation time via `SubnetMappings`. It is baked into the Talos machine configs as the Kubernetes API endpoint before any instances are launched.
- Security groups allow TCP 6443 and TCP 50000 only from the VPC CIDR (`10.0.0.0/16`). NLB health checks originate from within the VPC, so this is sufficient. Direct internet access to the private instances is not possible.
- The NAT Gateway gives private instances outbound internet access (container image pulls, time sync, etc.) without exposing any inbound surface.
- Each cluster gets its own VPC, so clusters are fully isolated from each other.

---

## Code Structure

```mermaid
flowchart TB
    subgraph cmd["cmd/cluster/  —  CLI (cobra)"]
        CLI["main.go\ncreate | delete | kubeconfig"]
    end

    subgraph internal["internal/"]
        subgraph cluster["cluster/"]
            MGR["manager.go\nCreate() / Delete()"]
            TYPES["types.go\nClusterConfig · ClusterState · NodeInfo"]
        end

        subgraph aws["aws/"]
            EC2pkg["ec2.go\nAllocateEIP · LaunchInstance\nWaitForInstanceRunning · TerminateInstances"]
            VPCpkg["vpc.go\nCreateNetworking · CreateSecurityGroups\nDeleteNetworking · WaitForNATGatewayDeleted"]
            NLBpkg["nlb.go\nCreateNLB · RegisterTargetsCP · DeleteNLB"]
            AMIpkg["ami.go\nFindTalosAMI"]
            CLIENTpkg["client.go\nNewEC2Client · NewELBv2Client"]
        end

        subgraph talos["talos/"]
            CFGpkg["config.go\nGenerateConfigs"]
            BOOTpkg["bootstrap.go\nWaitForTalosAPI · BootstrapCluster · FetchKubeconfig"]
        end
    end

    CLI --> MGR
    MGR --> TYPES
    MGR --> EC2pkg
    MGR --> VPCpkg
    MGR --> NLBpkg
    MGR --> AMIpkg
    MGR --> CLIENTpkg
    MGR --> CFGpkg
    MGR --> BOOTpkg
```

---

## Create Sequence

```mermaid
sequenceDiagram
    participant CLI as cmd/cluster
    participant MGR as cluster.Manager
    participant AWS as AWS EC2
    participant ELB as AWS ELBv2
    participant TAL as Talos SDK

    CLI->>MGR: Create(cfg)

    MGR->>AWS: FindTalosAMI
    AWS-->>MGR: ami-id

    MGR->>AWS: AllocateEIP  ← saved to state
    AWS-->>MGR: eip-id, publicIP (= ControlPlaneIP)

    MGR->>TAL: GenerateConfigs(ControlPlaneIP)
    TAL-->>MGR: controlplane.yaml, worker.yaml, talosconfig

    MGR->>AWS: CreateNetworking (VPC, subnets, IGW, NAT GW, route tables)
    Note over AWS: NAT GW takes ~60s to reach "available"
    AWS-->>MGR: netIDs  ← saved to state

    MGR->>AWS: CreateSecurityGroups
    AWS-->>MGR: cpSGID, workerSGID  ← saved to state

    MGR->>ELB: CreateNLB (EIP pinned to NLB)
    Note over ELB: NLB takes ~60s to reach "active"
    ELB-->>MGR: nlbARN, cpTGARN, talosTGARN  ← saved to state

    MGR->>AWS: LaunchInstance (control plane, private subnet)
    AWS-->>MGR: cpInstanceID  ← saved to state

    MGR->>AWS: WaitForInstanceRunning (cp)

    MGR->>ELB: RegisterTargetsCP (cp → both TGs)

    loop worker 0..N
        MGR->>AWS: LaunchInstance (worker, private subnet)
        AWS-->>MGR: workerID  ← saved to state
    end

    MGR->>AWS: WaitForInstanceRunning (all workers)

    MGR->>TAL: WaitForTalosAPI (polls NLB EIP:50000)
    Note over TAL: Talos boot takes ~3-5 min

    MGR->>TAL: BootstrapCluster (initiates etcd)

    MGR-->>CLI: ClusterState{status: "running"}, talosconfig
    CLI->>CLI: write <name>-state.json
    CLI->>CLI: write <name>-talosconfig
```

---

## Delete Sequence

```mermaid
sequenceDiagram
    participant CLI as cmd/cluster
    participant MGR as cluster.Manager
    participant AWS as AWS EC2
    participant ELB as AWS ELBv2

    CLI->>MGR: Delete(state)

    MGR->>AWS: TerminateInstances (cp + workers)
    Note over AWS: Waits up to 15 min for "terminated"

    MGR->>ELB: DeleteLoadBalancer (nlbARN)
    Note over ELB: Waits for NLB to be fully deleted
    ELB-->>MGR: ok
    MGR->>ELB: DeleteTargetGroup × 2
    ELB-->>MGR: ok

    MGR->>AWS: ReleaseAddress (cluster EIP)

    MGR->>AWS: DeleteNetworking
    Note over AWS: revoke SG rules → delete SGs<br/>delete private RT → delete public RT<br/>DeleteNatGateway → wait ~60s → release NAT EIP<br/>delete private subnet → delete public subnet<br/>detach IGW → delete IGW → delete VPC
    AWS-->>MGR: ok

    MGR-->>CLI: nil (success)
    CLI->>CLI: update <name>-state.json status: "deleted"
```

---

## State File

The `create` command writes a JSON state file after every significant resource allocation. This file is the input to `delete`. If `create` is interrupted mid-run, the partial state file is still valid — pass it to `delete` to clean up whatever was provisioned.

```
{
  config: {
    // user-supplied
    name, region, talos_version, kube_version,
    control_plane_type, worker_type, worker_count, ami_id,

    // networking
    vpc_id, public_subnet_id, subnet_id (private),
    igw_id, route_table_id (public), private_route_table_id,
    nat_gateway_id, nat_gateway_eip_id,
    control_plane_sg_id, worker_sg_id,

    // load balancer
    eip_id, control_plane_ip,
    nlb_arn, cp_target_group_arn, talos_target_group_arn,

    // instances
    control_plane_id, worker_ids[]
  },
  status: "creating" | "running" | "deleting" | "deleted"
}
```
