package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type renovateConfig struct {
	CustomManagers []renovateCustomManager `json:"customManagers"`
	PackageRules   []renovatePackageRule   `json:"packageRules"`
}

type renovateCustomManager struct {
	CustomType          string   `json:"customType"`
	Description         string   `json:"description"`
	ManagerFilePatterns []string `json:"managerFilePatterns"`
	MatchStrings        []string `json:"matchStrings"`
	DatasourceTemplate  string   `json:"datasourceTemplate"`
	VersioningTemplate  string   `json:"versioningTemplate"`
}

type renovatePackageRule struct {
	Description       string            `json:"description"`
	MatchFileNames    []string          `json:"matchFileNames"`
	MatchPackageNames []string          `json:"matchPackageNames"`
	MatchDepNames     []string          `json:"matchDepNames"`
	MatchUpdateTypes  []string          `json:"matchUpdateTypes"`
	MatchManagers     []string          `json:"matchManagers"`
	Constraints       map[string]string `json:"constraints"`
	Enabled           *bool             `json:"enabled"`
}

func loadRenovateConfig(t *testing.T, path string) renovateConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	var cfg renovateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to parse %s: %v", path, err)
	}
	return cfg
}

func findCUDAManager(t *testing.T, cfg renovateConfig) renovateCustomManager {
	t.Helper()
	targetPattern := `/^docker/unified/build-image\.sh$/`
	for _, mgr := range cfg.CustomManagers {
		if slices.Contains(mgr.ManagerFilePatterns, targetPattern) {
			return mgr
		}
	}
	t.Fatalf("customManager with managerFilePatterns containing %q not found in renovate.json", targetPattern)
	return renovateCustomManager{}
}

func findCUDAPackageRule(t *testing.T, cfg renovateConfig) (renovatePackageRule, int) {
	t.Helper()
	for idx, rule := range cfg.PackageRules {
		if slices.Contains(rule.MatchFileNames, "docker/unified/build-image.sh") &&
			slices.Contains(rule.MatchDepNames, "nvidia/cuda") {
			return rule, idx
		}
	}
	t.Fatalf("packageRule matching file 'docker/unified/build-image.sh' and dep 'nvidia/cuda' not found in renovate.json")
	return renovatePackageRule{}, -1
}

func readBuildImageScript(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}

func checkCUDAManagerTemplates(t *testing.T, mgr renovateCustomManager) {
	t.Helper()
	if mgr.CustomType != "regex" {
		t.Errorf("customType = %q; want %q", mgr.CustomType, "regex")
	}
	if mgr.DatasourceTemplate != "docker" {
		t.Errorf("datasourceTemplate = %q; want %q", mgr.DatasourceTemplate, "docker")
	}
	if mgr.VersioningTemplate != "docker" {
		t.Errorf("versioningTemplate = %q; want %q", mgr.VersioningTemplate, "docker")
	}
	if len(mgr.MatchStrings) != 1 {
		t.Fatalf("matchStrings length = %d; want 1", len(mgr.MatchStrings))
	}
}

func compileCUDAManagerRegex(t *testing.T, mgr renovateCustomManager) *regexp.Regexp {
	t.Helper()
	mgrRegex, err := regexp.Compile(mgr.MatchStrings[0])
	if err != nil {
		t.Fatalf("failed to compile matchString %q: %v", mgr.MatchStrings[0], err)
	}
	return mgrRegex
}

func collectCUDAMatches(t *testing.T, script string, mgrRegex *regexp.Regexp) ([]string, []string, [][]string) {
	t.Helper()
	broadRe := regexp.MustCompile(`nvidia/cuda:[^"}\s]*@sha256:[a-f0-9]{64}`)

	var pinnedLiterals []string
	var managerMatches []string
	var managerSubmatches [][]string

	lines := strings.Split(script, "\n")
	for _, line := range lines {
		if strings.Contains(line, "<digest>") {
			continue
		}
		pinnedLiterals = append(pinnedLiterals, broadRe.FindAllString(line, -1)...)
		sub := mgrRegex.FindAllStringSubmatch(line, -1)
		for _, sm := range sub {
			managerMatches = append(managerMatches, sm[0])
			managerSubmatches = append(managerSubmatches, sm)
		}
	}

	if len(pinnedLiterals) < 1 {
		t.Fatalf("pinned literal count = %d; want at least 1", len(pinnedLiterals))
	}
	return pinnedLiterals, managerMatches, managerSubmatches
}

func comparePinnedLiterals(t *testing.T, pinned, matches []string) {
	t.Helper()
	if len(matches) != len(pinned) {
		t.Errorf("manager matched %d entries, want %d pinned literals", len(matches), len(pinned))
	}

	for i := 0; i < len(pinned); i++ {
		if i >= len(matches) {
			t.Errorf("pinned literal %d %q not matched by manager regex", i, pinned[i])
			continue
		}
		if matches[i] != pinned[i] {
			t.Errorf("match %d mismatch: got %q, want %q", i, matches[i], pinned[i])
		}
	}
	if len(matches) > len(pinned) {
		for i := len(pinned); i < len(matches); i++ {
			t.Errorf("unexpected manager match %d: %q", i, matches[i])
		}
	}
}

func getCaptureIndices(t *testing.T, mgrRegex *regexp.Regexp) (int, int, int) {
	t.Helper()
	depNameIdx := mgrRegex.SubexpIndex("depName")
	currentValueIdx := mgrRegex.SubexpIndex("currentValue")
	currentDigestIdx := mgrRegex.SubexpIndex("currentDigest")

	if depNameIdx == -1 {
		t.Fatalf("capture group 'depName' not found in manager matchString")
	}
	if currentValueIdx == -1 {
		t.Fatalf("capture group 'currentValue' not found in manager matchString")
	}
	if currentDigestIdx == -1 {
		t.Fatalf("capture group 'currentDigest' not found in manager matchString")
	}
	return depNameIdx, currentValueIdx, currentDigestIdx
}

func checkCUDACaptures(t *testing.T, mgrRegex *regexp.Regexp, submatches [][]string) {
	t.Helper()
	depNameIdx, currentValueIdx, currentDigestIdx := getCaptureIndices(t, mgrRegex)

	for i, sm := range submatches {
		depName := sm[depNameIdx]
		currentValue := sm[currentValueIdx]
		currentDigest := sm[currentDigestIdx]

		if depName != "nvidia/cuda" {
			t.Errorf("match %d: depName = %q; want %q", i, depName, "nvidia/cuda")
		}
		if currentValue == "" {
			t.Errorf("match %d: currentValue is empty", i)
		}
		if !strings.HasPrefix(currentDigest, "sha256:") || len(currentDigest) != 71 {
			t.Errorf("match %d: currentDigest = %q; want prefix 'sha256:' and length 71 (got length %d)",
				i, currentDigest, len(currentDigest))
		}
	}
}

func TestRenovateCUDAManagerMatchesDigestTable(t *testing.T) {
	cfg := loadRenovateConfig(t, "renovate.json")
	mgr := findCUDAManager(t, cfg)
	checkCUDAManagerTemplates(t, mgr)

	mgrRegex := compileCUDAManagerRegex(t, mgr)
	buildScript := readBuildImageScript(t, "docker/unified/build-image.sh")
	pinnedLiterals, managerMatches, managerSubmatches := collectCUDAMatches(t, buildScript, mgrRegex)

	comparePinnedLiterals(t, pinnedLiterals, managerMatches)
	checkCUDACaptures(t, mgrRegex, managerSubmatches)
}

func checkRuleDisabled(t *testing.T, ruleIdx int, enabled *bool) {
	t.Helper()
	if enabled == nil {
		t.Errorf("packageRule[%d] enabled field is nil; want enabled == false", ruleIdx)
	} else if *enabled {
		t.Errorf("packageRule[%d] enabled = %t; want false", ruleIdx, *enabled)
	}
}

func checkUpdateTypesExact(t *testing.T, ruleIdx int, matchUpdateTypes []string) {
	t.Helper()
	expectedSet := map[string]bool{
		"major": true,
		"minor": true,
		"patch": true,
	}
	seenTypes := make(map[string]int)
	for _, ut := range matchUpdateTypes {
		seenTypes[ut]++
		if !expectedSet[ut] {
			t.Errorf("packageRule[%d] unexpected matchUpdateType %q (only major, minor, patch allowed)", ruleIdx, ut)
		}
	}
	for exp := range expectedSet {
		if seenTypes[exp] == 0 {
			t.Errorf("packageRule[%d] missing required matchUpdateType %q", ruleIdx, exp)
		}
	}
	if len(matchUpdateTypes) != len(expectedSet) {
		t.Errorf("packageRule[%d] matchUpdateTypes length = %d; want %d (%v)",
			ruleIdx, len(matchUpdateTypes), len(expectedSet), matchUpdateTypes)
	}
	if seenTypes["digest"] > 0 || seenTypes["pinDigest"] > 0 {
		t.Errorf("packageRule[%d] matchUpdateTypes must not contain 'digest' or 'pinDigest'", ruleIdx)
	}
}

func checkNoOtherRuleDisablesDigest(t *testing.T, cfg renovateConfig, ruleIdx int) {
	t.Helper()
	for i, otherRule := range cfg.PackageRules {
		if i == ruleIdx {
			continue
		}
		if slices.Contains(otherRule.MatchFileNames, "docker/unified/build-image.sh") {
			if otherRule.Enabled != nil && !*otherRule.Enabled {
				if slices.Contains(otherRule.MatchUpdateTypes, "digest") {
					t.Errorf("other packageRule[%d] (%q) matches docker/unified/build-image.sh and disables digest updates",
						i, otherRule.Description)
				}
			}
		}
	}
}

func TestRenovateCUDAPolicyDigestOnly(t *testing.T) {
	cfg := loadRenovateConfig(t, "renovate.json")
	rule, ruleIdx := findCUDAPackageRule(t, cfg)

	checkRuleDisabled(t, ruleIdx, rule.Enabled)
	checkUpdateTypesExact(t, ruleIdx, rule.MatchUpdateTypes)
	checkNoOtherRuleDisablesDigest(t, cfg, ruleIdx)
}

type unifiedCUDADefaults struct {
	Version      string
	DevelImage   string
	RuntimeImage string
}

func parseScriptDefaultCUDAVersion(script string) (string, error) {
	re := regexp.MustCompile(`(?m)^\s*\*\)\s*[\r\n]+\s*CUDA_VERSION="\$\{CUDA_VERSION:-([^"}]+)\}"`)
	sub := re.FindStringSubmatch(script)
	if len(sub) < 2 {
		return "", fmt.Errorf("default CUDA_VERSION in *) branch not found in script")
	}
	return sub[1], nil
}

func parseScriptCUDAImageRefs(script string, version string) (string, string, error) {
	blockRe, err := regexp.Compile(`(?ms)^\s*` + regexp.QuoteMeta(version) + `\)\s*$(.*?);;`)
	if err != nil {
		return "", "", fmt.Errorf("compile version case regex: %w", err)
	}
	blockSub := blockRe.FindStringSubmatch(script)
	if len(blockSub) < 2 {
		return "", "", fmt.Errorf("case entry for CUDA_VERSION %q not found in script", version)
	}
	block := blockSub[1]

	develRe := regexp.MustCompile(`CUDA_DEVEL_IMAGE="\$\{CUDA_DEVEL_IMAGE:-([^"}]+)\}"`)
	develSub := develRe.FindStringSubmatch(block)
	if len(develSub) < 2 {
		return "", "", fmt.Errorf("CUDA_DEVEL_IMAGE for %q not found in script", version)
	}

	runtimeRe := regexp.MustCompile(`CUDA_RUNTIME_IMAGE="\$\{CUDA_RUNTIME_IMAGE:-([^"}]+)\}"`)
	runtimeSub := runtimeRe.FindStringSubmatch(block)
	if len(runtimeSub) < 2 {
		return "", "", fmt.Errorf("CUDA_RUNTIME_IMAGE for %q not found in script", version)
	}

	return develSub[1], runtimeSub[1], nil
}

func parseBuildScriptCUDADefaults(script string) (unifiedCUDADefaults, error) {
	version, err := parseScriptDefaultCUDAVersion(script)
	if err != nil {
		return unifiedCUDADefaults{}, err
	}
	devel, runtime, err := parseScriptCUDAImageRefs(script, version)
	if err != nil {
		return unifiedCUDADefaults{}, err
	}
	return unifiedCUDADefaults{
		Version:      version,
		DevelImage:   devel,
		RuntimeImage: runtime,
	}, nil
}

func parseDockerfileArg(content string, argName string) (string, error) {
	re, err := regexp.Compile(`(?m)^\s*ARG\s+` + regexp.QuoteMeta(argName) + `=([^\s]+)`)
	if err != nil {
		return "", err
	}
	sub := re.FindStringSubmatch(content)
	if len(sub) < 2 {
		return "", fmt.Errorf("ARG %s default not found in Dockerfile", argName)
	}
	return sub[1], nil
}

func verifyUnifiedCUDADefaults(scriptContent, baseDockerfile, runtimeDockerfile string) error {
	defaults, err := parseBuildScriptCUDADefaults(scriptContent)
	if err != nil {
		return fmt.Errorf("parse build-image.sh: %w", err)
	}

	baseDevel, err := parseDockerfileArg(baseDockerfile, "CUDA_DEVEL_IMAGE")
	if err != nil {
		return fmt.Errorf("parse base-cuda.Dockerfile: %w", err)
	}
	if baseDevel != defaults.DevelImage {
		return fmt.Errorf("base-cuda.Dockerfile CUDA_DEVEL_IMAGE = %q; want %q", baseDevel, defaults.DevelImage)
	}

	runtimeVer, err := parseDockerfileArg(runtimeDockerfile, "CUDA_VERSION")
	if err != nil {
		return fmt.Errorf("parse runtime.Dockerfile: %w", err)
	}
	if runtimeVer != defaults.Version {
		return fmt.Errorf("runtime.Dockerfile CUDA_VERSION = %q; want %q", runtimeVer, defaults.Version)
	}

	runtimeImg, err := parseDockerfileArg(runtimeDockerfile, "CUDA_RUNTIME_IMAGE")
	if err != nil {
		return fmt.Errorf("parse runtime.Dockerfile: %w", err)
	}
	if runtimeImg != defaults.RuntimeImage {
		return fmt.Errorf("runtime.Dockerfile CUDA_RUNTIME_IMAGE = %q; want %q", runtimeImg, defaults.RuntimeImage)
	}

	return nil
}

func runCUDADefaultsNegativeCases(t *testing.T, scriptContent, baseContent, runtimeContent string, defaults unifiedCUDADefaults) {
	t.Helper()
	cases := []struct {
		name        string
		script      string
		base        string
		runtime     string
		errContains string
	}{
		{
			name:        "stale_runtime_version",
			script:      scriptContent,
			base:        baseContent,
			runtime:     strings.Replace(runtimeContent, "ARG CUDA_VERSION="+defaults.Version, "ARG CUDA_VERSION=12.9.0", 1),
			errContains: "runtime.Dockerfile CUDA_VERSION",
		},
		{
			name:        "stale_devel_image",
			script:      scriptContent,
			base:        strings.Replace(baseContent, "ARG CUDA_DEVEL_IMAGE="+defaults.DevelImage, "ARG CUDA_DEVEL_IMAGE=nvidia/cuda:stale-devel@sha256:0000", 1),
			runtime:     runtimeContent,
			errContains: "base-cuda.Dockerfile CUDA_DEVEL_IMAGE",
		},
		{
			name:        "stale_runtime_image",
			script:      scriptContent,
			base:        baseContent,
			runtime:     strings.Replace(runtimeContent, "ARG CUDA_RUNTIME_IMAGE="+defaults.RuntimeImage, "ARG CUDA_RUNTIME_IMAGE=nvidia/cuda:stale-runtime@sha256:0000", 1),
			errContains: "runtime.Dockerfile CUDA_RUNTIME_IMAGE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkErr := verifyUnifiedCUDADefaults(tc.script, tc.base, tc.runtime)
			if checkErr == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errContains)
			}
			if !strings.Contains(checkErr.Error(), tc.errContains) {
				t.Fatalf("got error %q; want substring %q", checkErr.Error(), tc.errContains)
			}
		})
	}
}

func TestUnifiedDocker_CUDADefaultsMatchBuildScript(t *testing.T) {
	scriptContent := readBuildImageScript(t, "docker/unified/build-image.sh")
	baseContent := readBuildImageScript(t, "docker/unified/base-cuda.Dockerfile")
	runtimeContent := readBuildImageScript(t, "docker/unified/runtime.Dockerfile")

	t.Run("LiveFilesMatch", func(t *testing.T) {
		if err := verifyUnifiedCUDADefaults(scriptContent, baseContent, runtimeContent); err != nil {
			t.Fatalf("live files drift: %v", err)
		}
	})

	t.Run("NegativeMismatchDetection", func(t *testing.T) {
		defaults, err := parseBuildScriptCUDADefaults(scriptContent)
		if err != nil {
			t.Fatalf("failed to parse script defaults: %v", err)
		}
		runCUDADefaultsNegativeCases(t, scriptContent, baseContent, runtimeContent, defaults)
	})
}

func checkNpmArtifactStepPinnedBelow12(rules []renovatePackageRule) error {
	var matchingIndices []int
	for idx, r := range rules {
		if slices.Contains(r.MatchManagers, "npm") && r.Constraints != nil && r.Constraints["npm"] == "<12" {
			matchingIndices = append(matchingIndices, idx)
		}
	}
	if len(matchingIndices) == 0 {
		return fmt.Errorf("expected exactly one rule with matchManagers containing \"npm\" and constraints[\"npm\"] == \"<12\", found 0")
	}
	if len(matchingIndices) > 1 {
		return fmt.Errorf("expected exactly one rule with matchManagers containing \"npm\" and constraints[\"npm\"] == \"<12\", found %d (rule indices: %v)", len(matchingIndices), matchingIndices)
	}
	return nil
}

func TestRenovate_NpmArtifactStepPinnedBelow12(t *testing.T) {
	cfg := loadRenovateConfig(t, "renovate.json")
	if err := checkNpmArtifactStepPinnedBelow12(cfg.PackageRules); err != nil {
		t.Fatalf("renovate.json: %v", err)
	}

	t.Run("NegativeCases", func(t *testing.T) {
		cases := []struct {
			name  string
			rules []renovatePackageRule
		}{
			{
				name:  "no_rules",
				rules: []renovatePackageRule{},
			},
			{
				name: "wrong_manager",
				rules: []renovatePackageRule{
					{
						MatchManagers: []string{"pip"},
						Constraints:   map[string]string{"npm": "<12"},
					},
				},
			},
			{
				name: "wrong_constraint_version",
				rules: []renovatePackageRule{
					{
						MatchManagers: []string{"npm"},
						Constraints:   map[string]string{"npm": "<11"},
					},
				},
			},
			{
				name: "missing_constraint_map",
				rules: []renovatePackageRule{
					{
						MatchManagers: []string{"npm"},
					},
				},
			},
			{
				name: "duplicate_rules",
				rules: []renovatePackageRule{
					{
						MatchManagers: []string{"npm"},
						Constraints:   map[string]string{"npm": "<12"},
					},
					{
						MatchManagers: []string{"npm"},
						Constraints:   map[string]string{"npm": "<12"},
					},
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := checkNpmArtifactStepPinnedBelow12(tc.rules); err == nil {
					t.Errorf("expected error for negative case %q, got nil", tc.name)
				}
			})
		}
	})
}
