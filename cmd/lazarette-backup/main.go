// Command lazarette-backup writes verified deterministic snapshots of a
// Lazarette store directory, restores snapshots into an empty target directory,
// and executes isolated scratch-directory restore drills.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ClearedStack/lazarette/internal/store"
)

type summaryOutput struct {
	Op            string `json:"op"`
	Repos         int    `json:"repos"`
	Manifests     int    `json:"manifests"`
	Blobs         int    `json:"blobs"`
	Bytes         int64  `json:"bytes"`
	ArchiveSHA256 string `json:"archiveSha256,omitempty"`
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lazarette-backup: subcommand required (snapshot, restore, or drill)")
		return 2
	}
	switch args[0] {
	case "snapshot":
		return runSnapshot(ctx, args[1:], stdout, stderr)
	case "restore":
		return runRestore(args[1:], stdin, stdout, stderr)
	case "drill":
		return runDrill(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "lazarette-backup: unfamiliar subcommand %q (want snapshot, restore, or drill)\n", args[0])
		return 2
	}
}

func runSnapshot(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fsFlags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fsFlags.SetOutput(stderr)
	dataDir := fsFlags.String("data", strings.TrimSpace(os.Getenv("LAZARETTE_DATA")), "Lazarette store root directory (required)")
	outPath := fsFlags.String("out", "", "output .tar.gz path or '-' for stdout (required unless -upload-url is set)")
	scratchDir := fsFlags.String("scratch", "", "scratch directory for temporary staging")
	uploadURL := fsFlags.String("upload-url", strings.TrimSpace(os.Getenv("LAZARETTE_BACKUP_URL")), "optional HTTP/HTTPS PUT URL for off-node backup upload")
	tokenFile := fsFlags.String("upload-token-file", "", "optional file containing Bearer token for -upload-url")
	timeout := fsFlags.Duration("timeout", 5*time.Minute, "HTTP timeout for -upload-url")
	if err := fsFlags.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" || (*outPath == "" && *uploadURL == "") || *timeout <= 0 || fsFlags.NArg() != 0 {
		fmt.Fprintln(stderr, "lazarette-backup snapshot: -data and at least one of -out or -upload-url are required")
		return 2
	}

	if *outPath == "-" && *uploadURL == "" {
		stats, err := store.WriteSnapshotFromDir(*dataDir, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: %v\n", err)
			return 1
		}
		_ = stats
		return 0
	}

	stageBase := *scratchDir
	if stageBase == "" {
		if *outPath != "" && *outPath != "-" {
			stageBase = filepath.Dir(*outPath)
		} else {
			stageBase = os.TempDir()
		}
	}
	tmp, err := os.CreateTemp(stageBase, "lazarette-snapshot-*.tar.gz")
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup snapshot: create staging file: %v\n", err)
		return 1
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	hasher := sha256.New()
	stats, err := store.WriteSnapshotFromDir(*dataDir, io.MultiWriter(tmp, hasher))
	if err != nil {
		tmp.Close()
		fmt.Fprintf(stderr, "lazarette-backup snapshot: %v\n", err)
		return 1
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		fmt.Fprintf(stderr, "lazarette-backup snapshot: sync archive: %v\n", err)
		return 1
	}
	if err := tmp.Close(); err != nil {
		fmt.Fprintf(stderr, "lazarette-backup snapshot: close archive: %v\n", err)
		return 1
	}
	archiveDigest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	if *uploadURL != "" {
		token, err := readOptionalToken(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: read upload token: %v\n", err)
			return 1
		}
		if err := uploadArchive(ctx, *uploadURL, token, archiveDigest, tmpName, *timeout); err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: upload failed: %v\n", err)
			return 1
		}
	}

	if *outPath == "-" {
		f, err := os.Open(tmpName)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: open staged archive: %v\n", err)
			return 1
		}
		_, copyErr := io.Copy(stdout, f)
		f.Close()
		if copyErr != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: write stdout: %v\n", copyErr)
			return 1
		}
		return 0
	}

	if *outPath != "" {
		if err := os.Chmod(tmpName, 0o640); err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: chmod archive: %v\n", err)
			return 1
		}
		if err := moveFile(tmpName, *outPath); err != nil {
			fmt.Fprintf(stderr, "lazarette-backup snapshot: write %s: %v\n", *outPath, err)
			return 1
		}
	}

	writeSummary(stdout, summaryOutput{
		Op:            "snapshot",
		Repos:         stats.Repos,
		Manifests:     stats.Manifests,
		Blobs:         stats.Blobs,
		Bytes:         stats.Bytes,
		ArchiveSHA256: archiveDigest,
	})
	return 0
}

func runRestore(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fsFlags := flag.NewFlagSet("restore", flag.ContinueOnError)
	fsFlags.SetOutput(stderr)
	inPath := fsFlags.String("in", "", "input .tar.gz path or '-' for stdin (required)")
	dataDir := fsFlags.String("data", strings.TrimSpace(os.Getenv("LAZARETTE_DATA")), "empty destination store directory (required)")
	if err := fsFlags.Parse(args); err != nil {
		return 2
	}
	if *inPath == "" || *dataDir == "" || fsFlags.NArg() != 0 {
		fmt.Fprintln(stderr, "lazarette-backup restore: -in and -data are required")
		return 2
	}

	var r io.Reader = stdin
	if *inPath != "-" {
		f, err := os.Open(*inPath)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup restore: open %s: %v\n", *inPath, err)
			return 1
		}
		defer f.Close()
		r = f
	}

	stats, err := store.RestoreSnapshot(r, *dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup restore: %v\n", err)
		return 1
	}
	if _, err := store.Open(*dataDir); err != nil {
		fmt.Fprintf(stderr, "lazarette-backup restore: verify opened store: %v\n", err)
		return 1
	}

	writeSummary(stdout, summaryOutput{
		Op:        "restore",
		Repos:     stats.Repos,
		Manifests: stats.Manifests,
		Blobs:     stats.Blobs,
		Bytes:     stats.Bytes,
	})
	return 0
}

func runDrill(args []string, stdout, stderr io.Writer) int {
	fsFlags := flag.NewFlagSet("drill", flag.ContinueOnError)
	fsFlags.SetOutput(stderr)
	inPath := fsFlags.String("in", "", "snapshot .tar.gz file to verify (mutually exclusive with -data)")
	dataDir := fsFlags.String("data", "", "live store directory to snapshot and restore-verify (mutually exclusive with -in)")
	scratchDir := fsFlags.String("scratch", os.TempDir(), "scratch directory for isolated restore drill")
	if err := fsFlags.Parse(args); err != nil {
		return 2
	}
	if (*inPath == "" && *dataDir == "") || (*inPath != "" && *dataDir != "") || *scratchDir == "" || fsFlags.NArg() != 0 {
		fmt.Fprintln(stderr, "lazarette-backup drill: specify exactly one of -in or -data")
		return 2
	}

	workDir, err := os.MkdirTemp(*scratchDir, "lazarette-drill-*")
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup drill: create scratch workspace: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	archivePath := *inPath
	var archiveDigest string
	if *dataDir != "" {
		archivePath = filepath.Join(workDir, "snapshot.tar.gz")
		f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup drill: create scratch archive: %v\n", err)
			return 1
		}
		h := sha256.New()
		_, snapErr := store.WriteSnapshotFromDir(*dataDir, io.MultiWriter(f, h))
		closeErr := f.Close()
		if snapErr != nil {
			fmt.Fprintf(stderr, "lazarette-backup drill: snapshot live store: %v\n", snapErr)
			return 1
		}
		if closeErr != nil {
			fmt.Fprintf(stderr, "lazarette-backup drill: close scratch archive: %v\n", closeErr)
			return 1
		}
		archiveDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	}

	af, err := os.Open(archivePath)
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup drill: open archive: %v\n", err)
		return 1
	}
	restoreTarget := filepath.Join(workDir, "restored")
	stats, restoreErr := store.RestoreSnapshot(af, restoreTarget)
	af.Close()
	if restoreErr != nil {
		fmt.Fprintf(stderr, "lazarette-backup drill: restore verification failed: %v\n", restoreErr)
		return 1
	}

	st, err := store.Open(restoreTarget)
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup drill: open restored store: %v\n", err)
		return 1
	}
	repos, err := st.ListRepos()
	if err != nil {
		fmt.Fprintf(stderr, "lazarette-backup drill: list repositories: %v\n", err)
		return 1
	}
	for _, repo := range repos {
		retained, err := st.ListRetainedDigests(repo)
		if err != nil {
			fmt.Fprintf(stderr, "lazarette-backup drill: list retained digests for %s: %v\n", repo, err)
			return 1
		}
		for _, dStr := range retained {
			d, err := store.ParseDigest(dStr)
			if err != nil {
				fmt.Fprintf(stderr, "lazarette-backup drill: parse retained digest %s in %s: %v\n", dStr, repo, err)
				return 1
			}
			if _, err := st.GetManifest(repo, d); err != nil {
				fmt.Fprintf(stderr, "lazarette-backup drill: read retained manifest %s in %s: %v\n", dStr, repo, err)
				return 1
			}
		}
		if _, err := st.ListTags(repo); err != nil {
			fmt.Fprintf(stderr, "lazarette-backup drill: list tags for %s: %v\n", repo, err)
			return 1
		}
	}

	writeSummary(stdout, summaryOutput{
		Op:            "drill",
		Repos:         stats.Repos,
		Manifests:     stats.Manifests,
		Blobs:         stats.Blobs,
		Bytes:         stats.Bytes,
		ArchiveSHA256: archiveDigest,
	})
	return 0
}

func readOptionalToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func uploadArchive(ctx context.Context, rawURL, token, archiveDigest, filePath string, timeout time.Duration) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("upload URL must be http or https with a host")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Content-SHA256", archiveDigest)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	hc := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP status %d", res.StatusCode)
	}
	return nil
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func writeSummary(w io.Writer, s summaryOutput) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	fmt.Fprintln(w, string(raw))
}
