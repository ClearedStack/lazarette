# Lazarette specification

> Internal-only OCI v2 image store in the clearedstack namespace retaining the last 5 cosign-signed digests per app for instant rollback without Docker or GitHub image hosting.

**Status: design.** This is a specification. It describes how Lazarette is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/lazarette/](https://clearedstack.com/platform/lazarette/). To change the specification, change it here and say why.

## Overview & Role

Lazarette (`v0.1.0-draft`, owned by the Lazarette repository) is a clean-room MIT Go microservice running in the `clearedstack` control-plane namespace. It serves as the cluster's private **OCI v2 container image registry**, storing signed container images built by Keelworks and serving them to `containerd` on `k3s` nodes and Shipkeeper during reconciliations and rollbacks.

By running a purpose-built internal OCI store instead of Harbor or Docker Hub, ClearedStack keeps all container artifacts inside the cluster: **zero container images are ever pushed to or pulled from GitHub**.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Lazarette (`lazarette`) |
| **Namespace** | `clearedstack` (`control-plane`) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget (Requested)** | `64 MiB` |
| **Priority Class** | High (Control Plane — protected from eviction) |
| **Persistence** | Dedicated PersistentVolumeClaim (PVC) for OCI blobs & manifests |
| **Sub-Tools** | None (`[]`) |

## Interfaces & Data Contracts

Lazarette exposes a cluster-private HTTP interface (`ClusterIP` only, no public Ingress) implementing standard OCI Distribution Specification (`/v2/`) endpoints:

- **Image Push (Keelworks build jobs):** Accepts OCI image manifests, config blobs, and layers from ephemeral `ko` and rootless BuildKit `Job` pods, along with `cosign` signature artifacts.
- **Image Pull (`containerd` / `k3s`):** Serves signed image manifests and layers by immutable `sha256` digest when Shipkeeper deploys or rolls back workloads.
- **Retention & Digest Index (Shipkeeper):** Retains the **last 5 signed digests per app** so Shipkeeper can execute 1-click rollbacks from `https://clearedstack.com/platform/#registry` without triggering a rebuild.
- **Daily Garbage Collection:** Runs a scheduled cleanup pass to remove unreferenced layers and manifests older than the 5 retained digests per app, keeping PVC disk usage predictable on a single-node cluster.

## Sub-Tools & Failure Modes

Lazarette is a control-plane infrastructure service and does not host `tools/<case>/` deterministic sub-tools (`subTools: []`).

| Failure Mode | Trigger | System Behavior & Recovery |
| --- | --- | --- |
| **PVC disk pressure** | Rapidly accumulating image layers | Daily garbage collection prunes unreferenced blobs beyond the 5-digest retention window per app. |
| **Unsigned or mismatched digest** | Attempted pull or deploy of an unverified artifact | Shipkeeper and `cosign` verification reject unsigned digests before rollout; Keelworks must produce a valid signed build. |
| **Push / pull timeout (`E_TIMEOUT`)** | Transient network or storage stall during `Job` push | Build `Job` fails cleanly; Keelworks can re-run the deterministic build without leaving partial manifests active. |
| **Pod crash or restart** | Node pressure or OOM | High PriorityClass protects Lazarette from eviction; PVC preserves all committed digests across restarts. |

## Security & Clean-Room Boundaries

- **Cluster-private network boundary:** Lazarette has no public Ingress. Default-deny Kubernetes `NetworkPolicy` rules restrict access to internal cluster traffic only (Keelworks pushing, Shipkeeper inspecting digests, and `containerd` pulling).
- **Zero external image leakage:** Source code is mirrored to GitHub via Plainforge, but built OCI binaries and layers remain strictly inside the `clearedstack` namespace.
- **Unprivileged execution:** Runs as a non-root user with dropped Linux capabilities, no privilege escalation, and a read-only root filesystem (writable only on the mounted OCI storage PVC).
- **Immutable digest addressing:** Workloads are pinned in `app.yaml` by `sha256` digest rather than mutable `:latest` tags.

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
