package main

import (
	"encoding/json"
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
	Description       string   `json:"description"`
	MatchFileNames    []string `json:"matchFileNames"`
	MatchPackageNames []string `json:"matchPackageNames"`
	MatchDepNames     []string `json:"matchDepNames"`
	MatchUpdateTypes  []string `json:"matchUpdateTypes"`
	Enabled           *bool    `json:"enabled"`
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

func TestRenovateCUDAManagerMatchesDigestTable(t *testing.T) {
	cfg := loadRenovateConfig(t, "renovate.json")
	mgr := findCUDAManager(t, cfg)

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

	mgrRegex, err := regexp.Compile(mgr.MatchStrings[0])
	if err != nil {
		t.Fatalf("failed to compile matchString %q: %v", mgr.MatchStrings[0], err)
	}

	buildScript := readBuildImageScript(t, "docker/unified/build-image.sh")
	broadRe := regexp.MustCompile(`nvidia/cuda:[^"}\s]*@sha256:[a-f0-9]{64}`)

	var pinnedLiterals []string
	var managerMatches []string
	var managerSubmatches [][]string

	lines := strings.Split(buildScript, "\n")
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

	if len(managerMatches) != len(pinnedLiterals) {
		t.Errorf("manager matched %d entries, want %d pinned literals", len(managerMatches), len(pinnedLiterals))
	}

	for i := 0; i < len(pinnedLiterals); i++ {
		if i >= len(managerMatches) {
			t.Errorf("pinned literal %d %q not matched by manager regex", i, pinnedLiterals[i])
			continue
		}
		if managerMatches[i] != pinnedLiterals[i] {
			t.Errorf("match %d mismatch: got %q, want %q", i, managerMatches[i], pinnedLiterals[i])
		}
	}
	if len(managerMatches) > len(pinnedLiterals) {
		for i := len(pinnedLiterals); i < len(managerMatches); i++ {
			t.Errorf("unexpected manager match %d: %q", i, managerMatches[i])
		}
	}

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

	for i, sm := range managerSubmatches {
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

func TestRenovateCUDAPolicyDigestOnly(t *testing.T) {
	cfg := loadRenovateConfig(t, "renovate.json")
	rule, ruleIdx := findCUDAPackageRule(t, cfg)

	if rule.Enabled == nil {
		t.Errorf("packageRule[%d] enabled field is nil; want enabled == false", ruleIdx)
	} else if *rule.Enabled {
		t.Errorf("packageRule[%d] enabled = %t; want false", ruleIdx, *rule.Enabled)
	}

	expectedSet := map[string]bool{
		"major": true,
		"minor": true,
		"patch": true,
	}
	seenTypes := make(map[string]int)
	for _, ut := range rule.MatchUpdateTypes {
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
	if len(rule.MatchUpdateTypes) != len(expectedSet) {
		t.Errorf("packageRule[%d] matchUpdateTypes length = %d; want %d (%v)",
			ruleIdx, len(rule.MatchUpdateTypes), len(expectedSet), rule.MatchUpdateTypes)
	}
	if seenTypes["digest"] > 0 || seenTypes["pinDigest"] > 0 {
		t.Errorf("packageRule[%d] matchUpdateTypes must not contain 'digest' or 'pinDigest'", ruleIdx)
	}

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
