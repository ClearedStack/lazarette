package lazarette_test

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const modulePath = "github.com/ClearedStack/lazarette"

func TestStandardLibraryOnly(t *testing.T) {
	gomodBytes, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	gomod := string(gomodBytes)
	if !strings.Contains(gomod, "module "+modulePath+"\n") {
		t.Fatalf("go.mod does not declare module %s:\n%s", modulePath, gomod)
	}
	if strings.Contains(gomod, "require") {
		t.Fatalf("go.mod must have zero external dependencies:\n%s", gomod)
	}
	if _, err := os.Stat("go.sum"); err == nil {
		t.Fatal("go.sum should not exist when go.mod has zero external dependencies")
	}

	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(p, "/")
			if strings.Contains(first, ".") && p != modulePath && !strings.HasPrefix(p, modulePath+"/") {
				t.Errorf("%s imports %s, which is outside the Go standard library and %s", path, p, modulePath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go source files were checked")
	}
}

func TestRepositoryHygieneAndNoEmDashes(t *testing.T) {
	emDash := string(rune(0x2014))

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		base := strings.ToLower(d.Name())
		if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || base == ".dockerignore" {
			t.Errorf("Dockerfile artefact %s is forbidden; images are built with ko", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if utf8.Valid(data) && bytes.Contains(data, []byte(emDash)) {
			t.Errorf("%s contains an em dash (U+2014)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	lic, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	if !strings.Contains(string(lic), "MIT License") {
		t.Fatal("LICENSE does not contain MIT License")
	}

	compBytes, err := os.ReadFile("component.json")
	if err != nil {
		t.Fatalf("read component.json: %v", err)
	}
	var comp struct {
		Slug      string   `json:"slug"`
		Kind      string   `json:"kind"`
		Namespace string   `json:"namespace"`
		Image     string   `json:"image"`
		Memory    string   `json:"memory"`
		Version   string   `json:"version"`
		DependsOn []string `json:"dependsOn"`
		SubTools  []string `json:"subTools"`
	}
	if err := json.Unmarshal(compBytes, &comp); err != nil {
		t.Fatalf("parse component.json: %v", err)
	}
	if comp.Slug != "lazarette" || comp.Image != "lazarette" {
		t.Fatalf("unexpected slug/image in component.json: %+v", comp)
	}
	if comp.Kind != "control-plane" || comp.Namespace != "clearedstack" {
		t.Fatalf("unexpected kind/namespace in component.json: %+v", comp)
	}
	if comp.Memory != "64 MiB" || comp.Version != "v0.1.0-draft" {
		t.Fatalf("unexpected memory/version in component.json: %+v", comp)
	}
	if len(comp.DependsOn) != 0 || len(comp.SubTools) != 0 {
		t.Fatalf("expected empty dependsOn and subTools in component.json: %+v", comp)
	}
}

func TestKoAndKubernetesManifests(t *testing.T) {
	koBytes, err := os.ReadFile(".ko.yaml")
	if err != nil {
		t.Fatalf("read .ko.yaml: %v", err)
	}
	ko := string(koBytes)
	basePin := regexp.MustCompile(`(?m)^defaultBaseImage:\s+\S+@sha256:[0-9a-f]{64}\s*$`)
	if !basePin.MatchString(ko) {
		t.Fatalf(".ko.yaml must pin defaultBaseImage by sha256 digest:\n%s", ko)
	}
	for _, want := range []string{"linux/arm64", "main: ./cmd/lazarette", "main: ./cmd/lazarette-backup", "CGO_ENABLED=0", "-trimpath"} {
		if !strings.Contains(ko, want) {
			t.Errorf(".ko.yaml missing %q", want)
		}
	}

	prioBytes, err := os.ReadFile(filepath.Join("deploy", "priorityclass.yaml"))
	if err != nil {
		t.Fatalf("read deploy/priorityclass.yaml: %v", err)
	}
	prio := string(prioBytes)
	for _, want := range []string{"kind: PriorityClass", "name: clearedstack-high", "value: 100000", "preemptionPolicy: PreemptLowerPriority"} {
		if !strings.Contains(prio, want) {
			t.Errorf("deploy/priorityclass.yaml missing %q", want)
		}
	}

	workloadBytes, err := os.ReadFile(filepath.Join("deploy", "lazarette.yaml"))
	if err != nil {
		t.Fatalf("read deploy/lazarette.yaml: %v", err)
	}
	workload := string(workloadBytes)
	imgPin := regexp.MustCompile(`(?m)^\s*image:\s+lazarette\.clearedstack\.svc\.cluster\.local:5000/lazarette@sha256:[0-9a-f]{64}\s*$`)
	if !imgPin.MatchString(workload) {
		t.Fatalf("deploy/lazarette.yaml must pin Lazarette image by sha256 digest")
	}
	requiredWorkloadSnippets := []string{
		"kind: ServiceAccount",
		"kind: PersistentVolumeClaim",
		"kind: Service",
		"kind: Deployment",
		"namespace: clearedstack",
		"priorityClassName: clearedstack-high",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"type: RuntimeDefault",
		"key: kubernetes.io/arch",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		`values: ["cm3588-plus"]`,
		"operator: NotIn",
		"memory: 64Mi",
		"port: 5000",
		"storageClassName: longhorn",
	}
	for _, want := range requiredWorkloadSnippets {
		if !strings.Contains(workload, want) {
			t.Errorf("deploy/lazarette.yaml missing %q", want)
		}
	}
	if strings.Contains(workload, "kind: Secret") || strings.Contains(workload, "secretKeyRef") {
		t.Error("deploy/lazarette.yaml must not contain or reference embedded secrets")
	}

	backupBytes, err := os.ReadFile(filepath.Join("deploy", "backup-cronjob.yaml"))
	if err != nil {
		t.Fatalf("read deploy/backup-cronjob.yaml: %v", err)
	}
	backup := string(backupBytes)
	backupImgPin := regexp.MustCompile(`(?m)^\s*image:\s+lazarette\.clearedstack\.svc\.cluster\.local:5000/lazarette-backup@sha256:[0-9a-f]{64}\s*$`)
	if len(backupImgPin.FindAllString(backup, -1)) < 2 {
		t.Fatalf("deploy/backup-cronjob.yaml must pin lazarette-backup image by sha256 digest in both CronJobs")
	}
	requiredBackupSnippets := []string{
		"kind: CronJob",
		"name: lazarette-backup",
		"name: lazarette-restore-drill",
		"namespace: clearedstack",
		"priorityClassName: cs-build-low",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"type: RuntimeDefault",
		"key: kubernetes.io/arch",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		`values: ["cm3588-plus"]`,
		"operator: NotIn",
		"claimName: lazarette-data",
		"readOnly: true",
		"memory: 64Mi",
	}
	for _, want := range requiredBackupSnippets {
		if !strings.Contains(backup, want) {
			t.Errorf("deploy/backup-cronjob.yaml missing %q", want)
		}
	}
	if strings.Contains(backup, "kind: Secret") || strings.Contains(backup, "secretKeyRef") {
		t.Error("deploy/backup-cronjob.yaml must not contain or reference embedded secrets")
	}

	netpolBytes, err := os.ReadFile(filepath.Join("deploy", "networkpolicy.yaml"))
	if err != nil {
		t.Fatalf("read deploy/networkpolicy.yaml: %v", err)
	}
	netpol := string(netpolBytes)
	requiredNetpolSnippets := []string{
		"name: default-deny",
		"podSelector: {}",
		"name: lazarette",
		"namespace: clearedstack",
		"- Ingress",
		"- Egress",
		"egress: []",
		"app.kubernetes.io/name: keelworks",
		"app.kubernetes.io/managed-by: keelworks",
		"app.kubernetes.io/name: shipkeeper",
		"port: 5000",
	}
	for _, want := range requiredNetpolSnippets {
		if !strings.Contains(netpol, want) {
			t.Errorf("deploy/networkpolicy.yaml missing %q", want)
		}
	}

	kustBytes, err := os.ReadFile("kustomization.yaml")
	if err != nil {
		t.Fatalf("read kustomization.yaml: %v", err)
	}
	kust := string(kustBytes)
	deployEntries, err := os.ReadDir("deploy")
	if err != nil {
		t.Fatalf("read deploy directory: %v", err)
	}
	for _, e := range deployEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		rel := "deploy/" + e.Name()
		if !strings.Contains(kust, "- "+rel) {
			t.Errorf("kustomization.yaml does not list %s", rel)
		}
	}
}
