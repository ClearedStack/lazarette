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

> **Status: design.** The specification is not fully built. One first milestone works: pushing and pulling one image by digest. Everything else in [`SPEC.md`](SPEC.md) is listed below as not yet done.

Lazarette is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Lazarette is meant to be understood and used without the rest.

## What works today

One image can be pushed to Lazarette and pulled back, by `sha256` digest, through the standard OCI registry API, and it is kept on disk.

- **Push.** A client uploads the config and layer blobs (in one request, or in ordered chunks), then the image manifest, addressed by its digest. Every blob and manifest is checked against its digest before it is stored. A manifest is stored only once its config and all its layers are stored, so a half-finished push never leaves an image that can be pulled.
- **Pull.** A client fetches the manifest and blobs by digest, with `GET` or `HEAD`. Blobs can be fetched in byte ranges, so an interrupted pull can resume.
- **Storage.** Plain files under one directory. Writes are finished in a temporary place and moved into position in one step. Stopping the process and starting it again on the same directory loses nothing that was committed.
- **Refusals.** Anything else is refused with a fixed message and one of three public codes: `E_PARSE` (the data was broken), `E_NEEDS_SUBTOOL` (the data was valid and unfamiliar) or `E_TIMEOUT` (a request body stalled). Error messages are constants; nothing from the request or the machine is copied into them. Tags, image indexes, Docker-format manifests, deletes, tag listing and the catalog are all refused with `E_NEEDS_SUBTOOL`.
- **Footprint.** The service uses only the Go standard library. Pushing and pulling a 300 MiB blob peaked at about 7 MiB of memory in a manual run.

It has been driven by an existing client, [go-containerregistry](https://github.com/google/go-containerregistry) (the library behind `crane` and `ko`), inside the test suite. It has **not** been run in a Kubernetes cluster, built with `ko`, or pulled from by `containerd`.

### Run it

Needs Go 1.24 or newer.

```sh
go run ./cmd/lazarette -addr 127.0.0.1:5000 -data /path/to/an/empty/directory
```

`-data` is the only place Lazarette writes to. `-idle-timeout` (default `30s`) is how long a request body may stall before the request is refused with `E_TIMEOUT`.

### Test it

```sh
go test ./...
```

The tests need no network and no other software. They include:

- **Saved examples** in [`internal/registry/testdata/cases`](internal/registry/testdata/cases): one file per rule, each a list of requests and the exact status, headers and body expected back. Any change in behaviour fails `TestSavedExamples`.
- **Existing-client tests** that push and pull a made-up image over real HTTP with go-containerregistry, check every layer against its digest, and pull again after a restart.
- **Timeout tests** that stall a request body and check the `E_TIMEOUT` refusal and that nothing was stored.

### Not yet done

Each of these is a requirement of [`SPEC.md`](SPEC.md) that is not built:

| Requirement | State |
| --- | --- |
| `cosign` signature artifacts | Not built. They need either tags or the referrers API, and neither is handled. |
| Keeping the last 5 signed digests per app | Not built. Nothing records which digests belong to an app, or in what order. |
| Digest index for Shipkeeper | Not built. |
| Daily garbage collection | Not built. Nothing is ever deleted, including upload sessions a client abandons. |
| Multi-platform images (image index) and Docker-format manifests | Refused with `E_NEEDS_SUBTOOL`. |
| Tags | Refused with `E_NEEDS_SUBTOOL`. |
| Image built with `ko` | Not done. No `ko` configuration exists yet. |
| Kubernetes workload: non-root, read-only root filesystem, dropped capabilities, 64 MiB limit, high priority class, PVC, `ClusterIP` service, default-deny `NetworkPolicy` | Not written. The program itself writes only under `-data` and needs no privileges. |
| `E_TIMEOUT` for a stalled disk | Only a stalled request body is detected. A stalled disk is not. |
| Pulls by `containerd`, pushes by `ko` and BuildKit | Not exercised. |

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Lazarette does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Lazarette logo and mark, for light and dark backgrounds. |
| [`cmd/lazarette`](cmd/lazarette) | The program: flags, startup and shutdown. |
| [`internal/registry`](internal/registry) | The HTTP API, its fixed refusals, and the saved examples. |
| [`internal/store`](internal/store) | Blobs, manifests and upload sessions on disk. |
| [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | The libraries the tests use. The service itself uses none. |

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

`lazarette` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
