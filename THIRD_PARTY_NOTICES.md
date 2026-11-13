# Third-party notices

The Lazarette service is built on the Go standard library alone. Nothing below is compiled into the `lazarette` binary; `imports_test.go` fails the build's tests if that changes.

The tests use one existing registry client, so that the API is proven by a client Lazarette's authors did not write. It and the modules it pulls into the test binary are fetched by the Go toolchain at the versions pinned in `go.sum`. No code from them is copied into this repository.

| Module | Version | License |
| --- | --- | --- |
| `github.com/google/go-containerregistry` | v0.20.6 | Apache 2.0 |
| `github.com/containerd/stargz-snapshotter/estargz` | v0.16.3 | Apache 2.0 |
| `github.com/docker/cli` | v28.2.2 | Apache 2.0 |
| `github.com/docker/distribution` | v2.8.3 | Apache 2.0 |
| `github.com/docker/docker-credential-helpers` | v0.9.3 | MIT |
| `github.com/google/go-cmp` | v0.7.0 | BSD 3-Clause |
| `github.com/klauspost/compress` | v1.18.0 | BSD 3-Clause (parts Apache 2.0 and MIT) |
| `github.com/mitchellh/go-homedir` | v1.1.0 | MIT |
| `github.com/opencontainers/go-digest` | v1.0.0 | Apache 2.0 |
| `github.com/opencontainers/image-spec` | v1.1.1 | Apache 2.0 |
| `github.com/pkg/errors` | v0.9.1 | BSD 2-Clause |
| `github.com/sirupsen/logrus` | v1.9.3 | MIT |
| `github.com/vbatts/tar-split` | v0.12.1 | BSD 3-Clause |
| `golang.org/x/sync` | v0.15.0 | BSD 3-Clause |
| `golang.org/x/sys` | v0.33.0 | BSD 3-Clause |

Each module's full license text and notices ship inside the module itself, in the Go module cache.
