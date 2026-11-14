<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Lazarette" height="56">
  </picture>
</p>

<p align="center">
  <strong>Keeps built images inside the cluster, with recent versions ready for rollback.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy.** Built on the Go standard library alone, tested with saved HTTP examples and end-to-end registry flows, configured for `ko`, and packaged with hardened Kubernetes manifests in [`deploy/`](deploy). Items that depend on cluster setup or live sibling components are listed under [Cluster integration notes](#cluster-integration-notes).

Lazarette is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Lazarette is meant to be understood and used without the rest.

## What works today

Images, multi-platform image indexes, and `cosign` signature artifacts can be pushed to Lazarette and pulled back through the OCI Distribution v1.1 API, with per-app retention of the last 5 signed digests and scheduled garbage collection.

- **Push (`ko`, BuildKit, `cosign`).** Clients upload config and layer blobs (in one request or in ordered chunks), then push OCI image manifests (`application/vnd.oci.image.manifest.v1+json`) or OCI image indexes (`application/vnd.oci.image.index.v1+json`) by `sha256` digest. Every blob and manifest is verified against its digest and declared byte size before being atomically committed. A manifest or index is stored only once every referenced blob, child manifest, or subject manifest is already stored.
- **Pull (`containerd`, Shipkeeper, `cosign`).** Clients fetch manifests, image indexes, and blobs with `GET` or `HEAD`. Blobs support single HTTP byte ranges (`206 Partial Content`) so interrupted pulls can resume.
- **`cosign` signatures and OCI v1.1 referrers.** Lazarette accepts `cosign` signatures both via `PUT /v2/<name>/manifests/sha256-<64hex>.sig` (listed at `GET /v2/<name>/tags/list`) and via OCI v1.1 `subject` manifests (listed at `GET /v2/<name>/referrers/<digest>`, with optional `?artifactType=` filtering and `OCI-Subject` / `OCI-Filters-Applied` headers). Mutable human tags such as `:latest` or `:v1.2.3` are refused with `E_NEEDS_SUBTOOL`.
- **5-signed-digest retention & Shipkeeper index.** Whenever a stored image manifest or image index receives a `cosign` signature (`.sig` tag or signature referrer), Lazarette records that digest in the repository's retention ring (up to 5 digests, oldest to newest) and serves it at `GET /v2/<name>/digests/list`.
- **Daily garbage collection.** A background pass (`-gc-interval=24h`, `-gc-grace=1h`) prunes abandoned upload sessions, unsigned or aged-out manifests outside the 5-signed-digest window (together with their `.sig` tags and referrers), and unreferenced blobs whose modification time is older than the grace window so in-flight pushes are never disturbed.
- **Storage.** Plain files under one directory (`blobs/sha256`, `manifests/<repo>`, `uploads`). Writes finish in temporary files and rename into place with `fsync`. Restarting on the same directory preserves all committed blobs, manifests, tags, referrers, and retention rings.
- **Fixed refusals.** Every refusal uses standard OCI error JSON with constant messages and, where a public Lazarette code applies, one of `E_PARSE`, `E_NEEDS_SUBTOOL`, or `E_TIMEOUT` in `detail.lazarette`. Nothing from the request or host is echoed in error messages.
- **Zero external dependencies.** Both the service and the test suite use the Go standard library only (`go.mod` has zero `require` directives).

### Run it

Needs Go 1.24 or newer.

```sh
go run ./cmd/lazarette \
  -addr 127.0.0.1:5000 \
  -data /path/to/an/empty/directory \
  -idle-timeout 30s \
  -gc-interval 24h \
  -gc-grace 1h
```

Flags (each also configurable via environment variable when the CLI flag is omitted):

- `-addr` / `LAZARETTE_ADDR` (default `:5000`): TCP address to listen on.
- `-data` / `LAZARETTE_DATA` (required): directory where blobs, manifests, and upload sessions live; the only path Lazarette writes to.
- `-idle-timeout` / `LAZARETTE_IDLE_TIMEOUT` (default `30s`): how long a request body read may stall before the request is refused with `E_TIMEOUT`.
- `-gc-interval` / `LAZARETTE_GC_INTERVAL` (default `24h`, `0` disables): interval between scheduled garbage collection passes.
- `-gc-grace` / `LAZARETTE_GC_GRACE` (default `1h`): minimum age before unreferenced blobs, unsigned manifests, or unlocked uploads are eligible for removal.
- `-version`: print `v0.1.0-draft` and exit.

### Test it

```sh
go vet ./...
go test -race ./...
```

The tests need no network and no external binaries or libraries:

- **Saved examples** in [`internal/registry/testdata/cases`](internal/registry/testdata/cases): 25 fixture suites covering every OCI endpoint, digest rule, byte range, image index, `cosign` `.sig` tag (including OCI 1.1 referrers bridging), OCI v1.1 referrer, retained signed digest list, garbage collection pass, and fixed refusal.
- **Loopback HTTP & Keelworks/Shipkeeper wire tests** in [`internal/registry/client_test.go`](internal/registry/client_test.go): full push/pull/range/restart/index/`cosign`/GC flows plus exact ECDSA P-256 `cosign` SimpleSigning verification and `HEAD` digest checks matching Keelworks and Shipkeeper.
- **Native Go fuzz tests** in [`internal/registry/fuzz_test.go`](internal/registry/fuzz_test.go): fuzzes OCI route/range parsing (`FuzzParseRouteAndRange`) and manifest validation (`FuzzManifestValidation`), verifying that every refusal preserves the 3-public-code envelope.
- **Offline bootstrap & ECDSA P-256 signing tests** in [`internal/bootstrap/bootstrap_test.go`](internal/bootstrap/bootstrap_test.go) and [`cmd/lazarette-bootstrap/main_test.go`](cmd/lazarette-bootstrap/main_test.go): verifies byte-for-byte reproducibility, direct Lazarette push (`-push`), and stdlib ECDSA P-256 `cosign` SimpleSigning referrer attachment (`-sign-key`).
- **Timeout tests** in [`internal/registry/timeout_test.go`](internal/registry/timeout_test.go): stalled request body reads produce `408` with `E_TIMEOUT` and leave zero committed state.
- **Store, GC & snapshot/restore tests** in [`internal/store/store_test.go`](internal/store/store_test.go) and [`cmd/lazarette-backup/main_test.go`](cmd/lazarette-backup/main_test.go): atomic writes, concurrent upload locks, retention ordering, grace-window protection, deterministic `.tar.gz` snapshots (including read-only PVC mounts), and isolated restore drills.
- **Repository & deployment contract tests** in [`imports_test.go`](imports_test.go): verifies stdlib-only imports across all Go files, module path, `component.json`, `.ko.yaml`, `kustomization.yaml`, hardened `deploy/` manifests, and repository hygiene.

### Build, bootstrap & backup in Kubernetes

Lazarette is built straight from Go source with [`ko`](.ko.yaml) (no `Dockerfile`). Because Lazarette itself is the cluster's image registry, the very first `lazarette` image on a fresh cluster is built to a deterministic OCI Image Layout tarball and imported into `k3s` `containerd` on the `arm64` node. Either `ko` or the zero-dependency [`cmd/lazarette-bootstrap`](cmd/lazarette-bootstrap) helper can produce the tarball offline, and once Lazarette is running `cmd/lazarette-bootstrap` can also push and `cosign`-sign bootstrap images (`lazarette`, `lazarette-backup`, `shipkeeper`) directly into Lazarette:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o lazarette ./cmd/lazarette
DIGEST=$(go run ./cmd/lazarette-bootstrap -binary ./lazarette -out ./lazarette.tar)
k3s ctr images import ./lazarette.tar

# Once Lazarette is running on the node, push and cosign-sign the bootstrap image:
go run ./cmd/lazarette-bootstrap \
  -binary ./lazarette \
  -push http://127.0.0.1:5000 \
  -sign-key /path/to/cosign.key
```

Replace the bootstrap placeholder digests in [`deploy/lazarette.yaml`](deploy/lazarette.yaml) and [`deploy/backup-cronjob.yaml`](deploy/backup-cronjob.yaml) before applying [`kustomization.yaml`](kustomization.yaml) (`kubectl apply -k .`). Stateful control-plane services (`logbook`, `plainforge`, `lazarette`) remain managed via their `deploy/` manifests while Shipkeeper observes their running digests read-only. Subsequent releases are built, `cosign`-signed, and pushed into Lazarette by Keelworks, while [`cmd/lazarette-backup`](cmd/lazarette-backup) runs nightly read-only PVC snapshots and monthly isolated scratch restore drills.

### Cluster integration notes

| Item | State |
| --- | --- |
| `PriorityClass` scheduling | `lazarette` runs under `clearedstack-high` ([`deploy/priorityclass.yaml`](deploy/priorityclass.yaml), matching Logbook and Plainforge); backup and restore-drill `CronJob`s run under `cs-build-low` (installed by Keelworks). |
| `k3s` `registries.yaml` for node pulls | Each `arm64` `k3s` node needs `/etc/rancher/k3s/registries.yaml` configured to talk plain HTTP to `lazarette.clearedstack.svc.cluster.local:5000` inside the cluster. |
| Stalled local disk detection | `-idle-timeout` detects stalled network request bodies; kernel disk I/O hangs rely on Kubernetes liveness probes and Longhorn volume health rather than user-space syscall cancellation. |
| Live cluster pushes/pulls | Exercised over loopback HTTP in `go test`; not yet exercised against live `ko`, BuildKit, `cosign`, and `k3s` `containerd` pods in-cluster. |

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Lazarette does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component's entry in the ClearedStack naming registry. |
| [`.ko.yaml`](.ko.yaml) | `ko` build configuration (`lazarette` and `lazarette-backup`) with digest-pinned static base image (`linux/arm64`). |
| [`kustomization.yaml`](kustomization.yaml) | Kustomize entrypoint listing all Kubernetes manifests in `deploy/`. |
| [`deploy/`](deploy) | Hardened Kubernetes manifests (`PriorityClass`, `ServiceAccount`, `PersistentVolumeClaim`, `Service`, `Deployment`, backup/drill `CronJob`s, and default-deny `NetworkPolicy`) for namespace `clearedstack`. |
| [`brand/`](brand) | The Lazarette logo and mark, for light and dark backgrounds. |
| [`cmd/lazarette`](cmd/lazarette) | The program: flags and `LAZARETTE_*` env configuration, HTTP server, scheduled garbage collection, startup and shutdown. |
| [`cmd/lazarette-bootstrap`](cmd/lazarette-bootstrap) | Offline OCI Image Layout tarball packager, direct Lazarette pusher, and pure-stdlib ECDSA P-256 `cosign` signer. |
| [`cmd/lazarette-backup`](cmd/lazarette-backup) | Deterministic store snapshot writer, verified restorer, and isolated scratch-directory restore-drill helper. |
| [`internal/bootstrap`](internal/bootstrap) | Deterministic stdlib-only OCI Image Layout v1.0 builder, OCI pusher, and ECDSA P-256 SimpleSigning signer. |
| [`internal/registry`](internal/registry) | The HTTP API, its fixed refusals, fuzz targets, and the saved examples. |
| [`internal/store`](internal/store) | Blobs, manifests, per-repo retention/tag/referrer indexes, upload sessions, garbage collection, and snapshots/restores on disk. |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `cs-registry`, then Stow |
| **Namespace** | `clearedstack` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/lazarette` |
| **Memory budget** | 64 MiB |
| **Depends on** | Nothing else in the platform |
| **Depended on by** | [Keelworks](https://github.com/ClearedStack/keelworks), [Shipkeeper](https://github.com/ClearedStack/shipkeeper) |

### Boundaries

- Standard OCI registry API, inside the cluster only
- Pushes from Keelworks's build jobs
- Pulls by the cluster's container runtime when Shipkeeper deploys

## Names

`lazarette` is this component's identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component's entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
