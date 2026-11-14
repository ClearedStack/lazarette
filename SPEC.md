# Lazarette specification

> Internal-only OCI v2 image store in the clearedstack namespace retaining the last 5 cosign-signed digests per app for instant rollback without Docker or GitHub image hosting.

**Status: ready to deploy.** The service, saved examples, scheduled garbage collector, `ko` build configuration, and Kubernetes manifests are built and tested.

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
| **Priority Class** | High (Control Plane - protected from eviction) |
| **Persistence** | Dedicated PersistentVolumeClaim (PVC) for OCI blobs & manifests |
| **Sub-Tools** | None (`[]`) |

## Interfaces & Data Contracts

Lazarette exposes a cluster-private HTTP interface (`ClusterIP` only, no public Ingress) implementing standard OCI Distribution Specification (`/v2/`) endpoints:

- **Image Push (Keelworks build jobs):** Accepts OCI image manifests (`application/vnd.oci.image.manifest.v1+json`), OCI image indexes (`application/vnd.oci.image.index.v1+json`), config blobs, and layers from ephemeral `ko` and rootless BuildKit `Job` pods, along with `cosign` signature artifacts.
- **Image Pull (`containerd` / `k3s`):** Serves signed image manifests, image indexes, and blobs (including HTTP byte ranges) by immutable `sha256` digest when Shipkeeper deploys or rolls back workloads.
- **Signature Artifacts (`cosign`):** Accepts `cosign` signatures both through the OCI v1.1 `subject` field (`GET /v2/<name>/referrers/<digest>`, with optional `?artifactType=` filtering and `OCI-Subject` / `OCI-Filters-Applied` headers) and through `cosign` signature tags (`PUT`, `GET`, and `HEAD` `/v2/<name>/manifests/sha256-<64hex>.sig`, plus `GET /v2/<name>/tags/list`). Arbitrary mutable tags such as `:latest` or `:v1.2.3` remain refused with `E_NEEDS_SUBTOOL`.
- **Retention & Digest Index (Shipkeeper):** Retains the **last 5 signed digests per app** so Shipkeeper can execute 1-click rollbacks from `https://clearedstack.com/platform/#registry` without triggering a rebuild. `GET` and `HEAD` `/v2/<name>/digests/list` return `{"name":"<name>","digests":["sha256:..."]}` with up to 5 retained signed digests in oldest-to-newest order (matching Shipkeeper's rollback ring).
- **Daily Garbage Collection:** Runs a scheduled cleanup pass (`-gc-interval=24h`, `-gc-grace=1h`) to remove abandoned upload sessions, unsigned or aged-out manifests outside the 5 retained signed digests per app (along with dangling `.sig` tags and referrers), and unreferenced blobs outside the grace window, keeping PVC disk usage predictable on a single-node cluster.

### Specification clarifications recorded during implementation

1. **Signature wire formats:** `cosign` v2 supports both OCI v1.1 `subject` referrers (Sigstore bundle `application/vnd.dev.sigstore.bundle.v0.3+json`, DSSE `application/vnd.dsse.envelope.v1+json`, and `application/vnd.dev.cosign.artifact.sig.v1+json`) and legacy `sha256-<64hex>.sig` tags carrying `application/vnd.dev.cosign.simplesigning.v1+json` layers. Supporting both keeps `ko` + `cosign` jobs working regardless of `COSIGN_EXPERIMENTAL` / OCI 1.1 mode while still refusing mutable human tags.
2. **Digest index endpoint (`/v2/<name>/digests/list`):** The original draft required retaining a 5-digest index for Shipkeeper without naming the HTTP path. Because OCI Distribution `/tags/list` lists tags rather than untagged `sha256` image digests, `GET /v2/<name>/digests/list` was added alongside `/v2/<name>/tags/list` to return the retained signed `sha256` digests in oldest-to-newest order.

## Sub-Tools & Failure Modes

Lazarette is a control-plane infrastructure service and does not host `tools/<case>/` deterministic sub-tools (`subTools: []`). Every 4xx/5xx refusal uses standard OCI error JSON; when a public Lazarette error code applies, `detail.lazarette` carries one of `E_PARSE`, `E_NEEDS_SUBTOOL`, or `E_TIMEOUT` with fixed constant messages.

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
