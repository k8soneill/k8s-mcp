# Architecture

## AWS Infrastructure

Each cluster is provisioned inside a dedicated VPC with a two-tier network: a public subnet for internet-facing infrastructure (NLB, NAT Gateway) and a private subnet for EC2 instances. No instance has a public IP address.

```mermaid
flowchart TB
    Internet(["Internet"])

    subgraph VPC["VPC — 10.0.0.0/16"]
        IGW[Internet Gateway]

        subgraph Public["Public Subnet — 10.0.1.0/24"]
            NLB["NLB / Elastic IP<br/>TCP 6443 · TCP 50000"]
            NAT[NAT Gateway]
        end

        subgraph Private["Private Subnet — 10.0.2.0/24 · no public IPs"]
            CP[Control Plane EC2]
            W["Workers × N EC2"]
        end
    end

    Internet -->|inbound| IGW
    IGW --> NLB
    NLB -->|k8s API 6443| CP
    NLB -->|Talos API 50000| CP
    CP -.->|outbound| NAT
    W -.->|outbound| NAT
    NAT -.-> IGW
    IGW -.->|outbound| Internet
```

Solid arrows = inbound path (Internet → NLB → Control Plane).
Dashed arrows = outbound path (instances → NAT Gateway → internet).

**Key design decisions:**

- The cluster's Elastic IP is pinned to the NLB at creation time. It is baked into the Talos machine configs as the Kubernetes API endpoint before any instances are launched.
- Security groups allow TCP 6443 and TCP 50000 only from the VPC CIDR (`10.0.0.0/16`). NLB health checks originate from within the VPC, so this is sufficient.
- The NAT Gateway gives private instances outbound internet access (container image pulls, time sync, etc.) without exposing any inbound surface.
- Each cluster gets its own VPC, so clusters are fully isolated from each other.

---

## Code Structure

```mermaid
flowchart TB
    subgraph cmd["cmd/cluster — CLI"]
        CLI["main.go<br/>create · delete · kubeconfig"]
    end

    subgraph mgmt["internal/cluster"]
        MGR["manager.go<br/>Create / Delete"]
        TYPES["types.go<br/>ClusterConfig · ClusterState"]
    end

    subgraph awspkg["internal/aws"]
        EC2F["ec2.go<br/>AllocateEIP · LaunchInstance<br/>WaitForRunning · Terminate"]
        VPCF["vpc.go<br/>CreateNetworking<br/>CreateSecurityGroups<br/>DeleteNetworking"]
        NLBF["nlb.go<br/>CreateNLB<br/>RegisterTargetsCP<br/>DeleteNLB"]
        AMIF["ami.go<br/>FindTalosAMI"]
    end

    subgraph talpkg["internal/talos"]
        CFGF["config.go<br/>GenerateConfigs"]
        BOOTF["bootstrap.go<br/>WaitForTalosAPI<br/>BootstrapCluster<br/>FetchKubeconfig"]
    end

    CLI --> MGR
    MGR --> TYPES
    MGR --> EC2F
    MGR --> VPCF
    MGR --> NLBF
    MGR --> AMIF
    MGR --> CFGF
    MGR --> BOOTF
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

    MGR->>AWS: AllocateEIP
    AWS-->>MGR: eip-id + publicIP (ControlPlaneIP)
    Note over MGR: saved to state file

    MGR->>TAL: GenerateConfigs(ControlPlaneIP)
    TAL-->>MGR: controlplane.yaml, worker.yaml, talosconfig

    MGR->>AWS: CreateNetworking
    Note over AWS: VPC, subnets, IGW, NAT GW, route tables<br/>NAT GW takes ~60s to reach available
    AWS-->>MGR: netIDs
    Note over MGR: saved to state file

    MGR->>AWS: CreateSecurityGroups
    AWS-->>MGR: cpSGID, workerSGID
    Note over MGR: saved to state file

    MGR->>ELB: CreateNLB (EIP pinned to NLB)
    Note over ELB: NLB takes ~60s to reach active
    ELB-->>MGR: nlbARN, cpTGARN, talosTGARN
    Note over MGR: saved to state file

    MGR->>AWS: LaunchInstance (control plane, private subnet)
    AWS-->>MGR: cpInstanceID
    Note over MGR: saved to state file

    MGR->>AWS: WaitForInstanceRunning (cp)
    MGR->>ELB: RegisterTargetsCP (cp in both TGs)

    loop worker 0..N
        MGR->>AWS: LaunchInstance (worker, private subnet)
        AWS-->>MGR: workerID
        Note over MGR: saved to state file
    end

    MGR->>AWS: WaitForInstanceRunning (all workers)

    MGR->>TAL: WaitForTalosAPI (polls NLB EIP:50000)
    Note over TAL: Talos boot takes ~3-5 min

    MGR->>TAL: BootstrapCluster (initiates etcd)

    MGR-->>CLI: ClusterState + talosconfig
    CLI->>CLI: write state.json + talosconfig
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
    Note over AWS: waits up to 15 min for terminated

    MGR->>ELB: DeleteLoadBalancer
    Note over ELB: waits for NLB fully deleted
    MGR->>ELB: DeleteTargetGroup x2

    MGR->>AWS: ReleaseAddress (cluster EIP)

    MGR->>AWS: DeleteNetworking
    Note over AWS: revoke SG rules, delete SGs<br/>delete route tables<br/>DeleteNatGateway + wait ~60s<br/>release NAT EIP<br/>delete subnets, IGW, VPC

    MGR-->>CLI: success
    CLI->>CLI: update state.json status deleted
```

---

## State File

The `create` command writes a JSON state file after every significant resource allocation. This file is the input to `delete`. If `create` is interrupted mid-run, the partial state file is still valid — pass it to `delete` to clean up whatever was provisioned.

```
{
  config: {
    // generated on create
    cluster_id,               // 10-char random hex — unique per cluster instance

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
