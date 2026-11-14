// Command lazarette-bootstrap packages a compiled static Linux binary into a
// deterministic OCI Image Layout v1.0 tarball for offline k3s containerd
// bootstrap (`k3s ctr images import`), optionally pushes the image into a
// running Lazarette server, and optionally signs it with an ECDSA P-256 cosign
// key so Keelworks and Shipkeeper verify the bootstrap digest.
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ClearedStack/lazarette/internal/bootstrap"
)

const defaultImageRef = "lazarette.clearedstack.svc.cluster.local:5000/lazarette"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lazarette-bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	binPath := fs.String("binary", "", "path to compiled static Linux binary (required)")
	outPath := fs.String("out", "", "path to output OCI tarball (required unless -push is set)")
	pushURL := fs.String("push", "", "optional Lazarette base URL (for example http://127.0.0.1:5000) to push into")
	repo := fs.String("repo", "", "optional repository name in Lazarette (defaults to the path in -ref)")
	signKeyPath := fs.String("sign-key", "", "optional PEM file holding ECDSA P-256 cosign private key (requires -push)")
	timeout := fs.Duration("timeout", 30*time.Second, "HTTP timeout for -push and -sign-key")
	imageRef := fs.String("ref", defaultImageRef, "repository reference recorded in OCI index and cosign payload")
	entry := fs.String("entry", "/lazarette", "entrypoint path inside the image rootfs")
	arch := fs.String("arch", "arm64", "target OCI architecture")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *binPath == "" || (*outPath == "" && *pushURL == "") || (*signKeyPath != "" && *pushURL == "") || *timeout <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "lazarette-bootstrap: -binary and at least one of -out or -push are required (-sign-key requires -push)")
		return 2
	}

	var priv *ecdsa.PrivateKey
	if *signKeyPath != "" {
		pemBytes, err := os.ReadFile(*signKeyPath)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-bootstrap: read signing key: %v\n", err)
			return 1
		}
		priv, err = bootstrap.ParsePrivateKey(pemBytes)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-bootstrap: invalid signing key: %v\n", err)
			return 1
		}
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
	if *outPath != "" {
		if err := os.WriteFile(*outPath, res.Tarball, 0o644); err != nil {
			fmt.Fprintf(stderr, "lazarette-bootstrap: write tarball: %v\n", err)
			return 1
		}
	}
	if *pushURL != "" {
		if _, err := bootstrap.PushAndSign(ctx, bootstrap.PushOptions{
			BaseURL:    *pushURL,
			Timeout:    *timeout,
			Repo:       *repo,
			DockerRef:  *imageRef,
			Result:     res,
			PrivateKey: priv,
		}); err != nil {
			fmt.Fprintf(stderr, "lazarette-bootstrap: push failed: %v\n", err)
			return 1
		}
	}
	fmt.Fprintln(stdout, res.ManifestDigest)
	return 0
}
