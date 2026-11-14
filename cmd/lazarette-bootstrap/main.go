// Command lazarette-bootstrap packages a compiled static Linux binary into a
// deterministic OCI Image Layout v1.0 tarball for offline k3s containerd
// bootstrap (`k3s ctr images import`) and prints its sha256 manifest digest.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ClearedStack/lazarette/internal/bootstrap"
)

const defaultImageRef = "lazarette.clearedstack.svc.cluster.local:5000/lazarette"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lazarette-bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	binPath := fs.String("binary", "", "path to compiled static Linux binary (required)")
	outPath := fs.String("out", "", "path to output OCI tarball (required)")
	imageRef := fs.String("ref", defaultImageRef, "repository reference recorded in OCI index annotations")
	entry := fs.String("entry", "/lazarette", "entrypoint path inside the image rootfs")
	arch := fs.String("arch", "arm64", "target OCI architecture")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *binPath == "" || *outPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "lazarette-bootstrap: -binary and -out are required, and no extra arguments are accepted")
		return 2
	}

	binData, err := os.ReadFile(*binPath)
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-bootstrap: read binary: %v\n", err)
		return 1
	}
	res, err := bootstrap.Build(bootstrap.Spec{
		Binary:     binData,
		ImageRef:   *imageRef,
		Entrypoint: *entry,
		Arch:       *arch,
	})
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-bootstrap: build tarball: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*outPath, res.Tarball, 0o644); err != nil {
		fmt.Fprintf(stderr, "lazarette-bootstrap: write tarball: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, res.ManifestDigest)
	return 0
}
