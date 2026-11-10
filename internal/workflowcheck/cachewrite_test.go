package workflowcheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkflowCheck_RepositoryWorkflowsWriteCachesOnlyFromMain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	findings, files, err := CheckDir(ctx, "../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	if files < 15 {
		t.Fatalf("checked %d workflows, want at least 15", files)
	}
	t.Logf("checked %d workflows; %d findings", files, len(findings))
	for i := 0; i < len(findings) && i < maxEntries; i++ {
		t.Error(findings[i].String())
	}
}

func TestWorkflowCheck_PlantedWritesAreReported(t *testing.T) {
	want := map[string]string{
		"go-default.yml":       "set cache: false",
		"hit-only.yml":         "add github.ref",
		"node-explicit.yml":    "remove cache",
		"node-default.yml":     "set package-manager-cache: false",
		"python-explicit.yml":  "remove cache",
		"not-pr.yml":           "add github.ref",
		"or-guard.yml":         "add github.ref",
		"negated-guard.yml":    "add github.ref",
		"combined-cache.yml":   "add github.ref",
		"stale-dispatch.yml":   "add github.ref",
		"qemu-default.yml":     "set cache-image: false",
		"buildx-default.yml":   "set cache-binary: false",
		"unknown.yml":          "classify example/unknown-action in cacheBehaviours",
		"run-gha.yml":          "add github.ref",
		"push-unfiltered.yml":  "set cache: false",
		"local-action.yml":     "classify ./local in cacheBehaviours",
		"docker-action.yml":    "classify docker://alpine:3 in cacheBehaviours",
		"push-tags.yml":        "set cache: false",
		"push-tags-ignore.yml": "set cache: false",
		"push-ignore.yml":      "set cache: false",
		"push-other.yml":       "set cache: false",
		"call-default.yml":     "set cache: false",
	}
	files := fixtureFiles(t, "bad")
	if len(files) != len(want) {
		t.Fatalf("bad fixtures: got %d, table names %d", len(files), len(want))
	}
	for i := 0; i < len(files) && i < maxEntries; i++ {
		file := files[i]
		t.Run(file.Name(), func(t *testing.T) {
			expected, ok := want[file.Name()]
			if !ok {
				t.Fatalf("fixture %s missing from expected table", file.Name())
			}
			findings := checkFixture(t, "bad", file.Name())
			if !containsDetail(findings, expected) {
				t.Fatalf("want Detail containing %q, got %v", expected, findings)
			}
		})
	}
}

func TestWorkflowCheck_GuardedWritesPass(t *testing.T) {
	files := fixtureFiles(t, "good")
	if len(files) == 0 {
		t.Fatal("no good fixtures checked")
	}
	for i := 0; i < len(files) && i < maxEntries; i++ {
		file := files[i]
		t.Run(file.Name(), func(t *testing.T) {
			if findings := checkFixture(t, "good", file.Name()); len(findings) != 0 {
				t.Fatalf("unexpected findings: %v", findings)
			}
		})
	}
}

func fixtureFiles(t *testing.T, kind string) []os.DirEntry {
	t.Helper()
	files, err := os.ReadDir(filepath.Join("testdata", kind))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > maxEntries {
		t.Fatalf("too many %s fixtures: %d", kind, len(files))
	}
	return files
}

func checkFixture(t *testing.T, kind, name string) []Finding {
	t.Helper()
	path := filepath.Join("testdata", kind, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := CheckWorkflow(path, data)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func containsDetail(findings []Finding, want string) bool {
	for i := 0; i < len(findings) && i < maxEntries; i++ {
		if strings.Contains(findings[i].Detail, want) {
			return true
		}
	}
	return false
}

func TestWorkflowCheck_MainRefGuard(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{mainRef, true},
		{"${{ " + mainRef + " }}", true},
		{"\n github.ref   ==   'refs/heads/main'  &&\n success() ", true},
		{"(" + mainRef + ")", true},
		{"success() && (" + mainRef + ") && github.event_name == 'push'", true},
		{"((success() && (" + mainRef + ")))", true},
		{mainRef + " && " + mainRef, true},
		{"github.ref != 'refs/heads/main'", false},
		{"github.ref == 'refs/heads/mainline'", false},
		{mainRef + " || success()", false},
		{"success() || (" + mainRef + ")", false},
		{"!(github.ref != 'refs/heads/main')", false},
		{"! (github.ref != 'refs/heads/main')", false},
		{"", false}, {"${{ }}", false}, {" \t\n ", false},
		{"contains('" + mainRef + "', 'x')", false},
		{"success(" + mainRef + ")", false},
		{mainRef + ")", false},
		{"(" + mainRef, false},
		{strings.Repeat("(", maxParentheses+1) + mainRef + strings.Repeat(")", maxParentheses+1), false},
		{strings.Repeat(" ", maxExpression) + mainRef, false},
	}
	for i := 0; i < len(cases) && i < maxEntries; i++ {
		tc := cases[i]
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			if got := mainRefGuard(tc.expr); got != tc.want {
				t.Errorf("mainRefGuard(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestWorkflowCheck_MalformedWorkflowErrors(t *testing.T) {
	findings, err := CheckWorkflow("broken.yml", []byte("jobs: [\n"))
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("want wrapped YAML error, got %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("malformed workflow produced findings: %v", findings)
	}
}

func TestWorkflowCheck_DirectoryBoundaries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, files, err := CheckDir(ctx, "testdata/good")
	if !errors.Is(err, context.Canceled) || files != 0 {
		t.Fatalf("canceled check: files=%d, err=%v", files, err)
	}
	_, _, err = CheckDir(context.Background(), "testdata/missing")
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("want wrapped missing directory error, got %v", err)
	}
}

func TestWorkflowCheck_TriggerForms(t *testing.T) {
	cases := []struct {
		trigger string
		want    int
	}{
		{"on: schedule", 0},
		{"on: [schedule]", 0},
		{"on: push", 1},
		{"on: [schedule, workflow_dispatch]", 1},
		{"on:\n  push:\n    branches: main", 1},
		{"on:\n  push:\n    branches: [main, other]", 1},
	}
	for i := 0; i < len(cases) && i < maxEntries; i++ {
		tc := cases[i]
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			data := tc.trigger + "\njobs:\n  test:\n    steps:\n      - uses: actions/cache/save@v6\n"
			findings, err := CheckWorkflow("trigger.yml", []byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tc.want {
				t.Fatalf("got %v, want %d findings", findings, tc.want)
			}
		})
	}
}
