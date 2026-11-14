package main

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildContainer_PickLlamaTag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script runs only on the Linux container builder; Windows checkouts may rewrite line endings")
	}
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable on PATH: %v", tool, err)
		}
	}
	cases := []struct {
		name, prefix, versions, want string
		wantErr                      bool
	}{
		{"v1_tags_ignored", "server-cuda", `[{"metadata":{"container":{"tags":["server-cuda-v1.0.0-b12359-b1b770d","server-cuda12-b12352","server-cuda-b12352","server-cuda-b12375"]}}}]`, "b12375\n", false},
		{"exact_prefix", "server", `[{"metadata":{"container":{"tags":["server-vulkan-b12380","server-cuda12-b12380","server-b12375"]}}}]`, "b12375\n", false},
		{"numeric_not_text_order", "server", `[{"metadata":{"container":{"tags":["server-b9999","server-b12375"]}}}]`, "b12375\n", false},
		{"untagged_and_null_tags", "server-intel", `[{"metadata":{"container":{"tags":[]}}},{},{"metadata":{"container":{"tags":null}}},{"metadata":{"container":{"tags":["server-intel-b12375"]}}}]`, "b12375\n", false},
		{"no_match", "server-rocm", `[{"metadata":{"container":{"tags":["server-cuda-b12375"]}}}]`, "", true},
		{"boundary_single_digit", "server-musa", `[{"metadata":{"container":{"tags":["server-musa-b0"]}}}]`, "b0\n", false},
		{"tag_text_kept", "server-vulkan", `[{"metadata":{"container":{"tags":["server-vulkan-b007","server-vulkan-b6"]}}}]`, "b007\n", false},
		{"non_digit_suffix", "server", `[{"metadata":{"container":{"tags":["server-b12a","server-bx"]}}}]`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "docker/llama-tag.sh", tc.prefix)
			cmd.Stdin = strings.NewReader(tc.versions)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if ctx.Err() != nil {
				t.Fatalf("script deadline: %v", ctx.Err())
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("script error = %v; wantErr %t; stderr: %s", err, tc.wantErr, stderr.String())
			}
			if string(out) != tc.want {
				t.Errorf("stdout = %q; want %q", out, tc.want)
			}
		})
	}
}
