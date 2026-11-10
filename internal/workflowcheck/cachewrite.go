// Package workflowcheck fails any workflow step that can write the GitHub
// Actions cache from a ref other than refs/heads/main.
package workflowcheck

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxEntries     = 4096
	maxExpression  = 16384
	maxParentheses = 256
	mainRef        = "github.ref == 'refs/heads/main'"
)

// Finding identifies an unsafe cache writer and the change it needs.
type Finding struct{ File, Job, Step, Detail string }

func (f Finding) String() string {
	return fmt.Sprintf("%s: job %s, step %s: %s", f.File, f.Job, f.Step, f.Detail)
}

type workflow struct {
	On   yaml.Node `yaml:"on"`
	Jobs yaml.Node `yaml:"jobs"`
}

type job struct {
	If    string `yaml:"if"`
	Uses  string `yaml:"uses"`
	Steps []step `yaml:"steps"`
}

type step struct {
	Name string    `yaml:"name"`
	If   string    `yaml:"if"`
	Uses string    `yaml:"uses"`
	Run  string    `yaml:"run"`
	With yaml.Node `yaml:"with"`
}

type namedNode struct {
	name string
	node *yaml.Node
}

type cacheKind int

const (
	none cacheKind = iota
	write
	inputGated
)

type cacheBehaviour struct {
	kind            cacheKind
	disabled, empty string
}

var cacheBehaviours = map[string]cacheBehaviour{
	"actions/checkout":                  {kind: none},                                                          // Only checks out source; no Actions cache.
	"actions/cache/restore":             {kind: none},                                                          // Restores only; no save post step.
	"docker/login-action":               {kind: none},                                                          // Registry login; no Actions cache.
	"peter-evans/repository-dispatch":   {kind: none},                                                          // Dispatches an event; no Actions cache.
	"sigstore/cosign-installer":         {kind: none},                                                          // Composite installer; no Actions cache.
	"anchore/sbom-action/download-syft": {kind: none},                                                          // Uses runner-local tool-cache only.
	"goreleaser/goreleaser-action":      {kind: none},                                                          // Uses runner-local tool-cache only.
	"actions/cache":                     {kind: write},                                                         // Uses @actions/cache, including a save post step.
	"actions/cache/save":                {kind: write},                                                         // Uses @actions/cache to save explicitly.
	"actions/stale":                     {kind: write},                                                         // Uses @actions/cache for state.
	"dataaxiom/ghcr-cleanup-action":     {kind: write},                                                         // Uses @actions/cache for manifests.
	"actions/setup-go":                  {kind: inputGated, disabled: "cache"},                                 // cache defaults to true.
	"actions/setup-node":                {kind: inputGated, disabled: "package-manager-cache", empty: "cache"}, // Both inputs enable caching.
	"actions/setup-python":              {kind: inputGated, empty: "cache"},                                    // cache opts into caching.
	"docker/setup-buildx-action":        {kind: inputGated, disabled: "cache-binary"},                          // cache-binary defaults to true.
	"docker/setup-qemu-action":          {kind: inputGated, disabled: "cache-image"},                           // cache-image defaults to true.
}

// CheckWorkflow checks one workflow without executing any of its steps.
func CheckWorkflow(name string, data []byte) ([]Finding, error) {
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("decode workflow %s: %w", name, err)
	}
	jobs, err := mapping(wf.Jobs)
	if err != nil {
		return nil, fmt.Errorf("read jobs in %s: %w", name, err)
	}
	var findings []Finding
	for i := 0; i < len(jobs) && i < maxEntries; i++ {
		found, err := checkJob(name, jobs[i], mainOnly(wf.On))
		if err != nil {
			return nil, fmt.Errorf("check job %s in %s: %w", jobs[i].name, name, err)
		}
		findings = append(findings, found...)
	}
	return findings, nil
}

func checkJob(file string, entry namedNode, workflowGuard bool) ([]Finding, error) {
	var j job
	if err := entry.node.Decode(&j); err != nil {
		return nil, fmt.Errorf("decode job: %w", err)
	}
	if j.Uses != "" {
		return nil, nil
	}
	if len(j.Steps) > maxEntries {
		return nil, fmt.Errorf("job exceeds %d steps", maxEntries)
	}
	guarded := workflowGuard || mainRefGuard(j.If)
	var findings []Finding
	for i := 0; i < len(j.Steps) && i < maxEntries; i++ {
		findings = append(findings, checkStep(file, entry.name, i, j.Steps[i], guarded)...)
	}
	return findings, nil
}

// CheckDir checks sorted YAML workflow files, observing cancellation per file.
// It reads through an os.Root, so a symlink that leaves dir is an error rather
// than a file read from elsewhere.
func CheckDir(ctx context.Context, dir string) (findings []Finding, files int, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("open workflows in %s: %w", dir, err)
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close workflows in %s: %w", dir, closeErr))
		}
	}()
	return checkRoot(ctx, dir, root)
}

func checkRoot(ctx context.Context, dir string, root *os.Root) (findings []Finding, files int, err error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, 0, fmt.Errorf("list workflows in %s: %w", dir, err)
	}
	if len(entries) > maxEntries {
		return nil, 0, fmt.Errorf("workflow directory %s exceeds %d entries", dir, maxEntries)
	}
	for i := 0; i < len(entries) && i < maxEntries; i++ {
		entry := entries[i]
		if !workflowFile(entry) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return findings, files, fmt.Errorf("check workflow %s: %w", entry.Name(), err)
		}
		name := filepath.Join(dir, entry.Name())
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			return findings, files, fmt.Errorf("read workflow %s: %w", name, err)
		}
		found, err := CheckWorkflow(name, data)
		if err != nil {
			return findings, files, fmt.Errorf("check workflow %s: %w", name, err)
		}
		findings = append(findings, found...)
		files++
	}
	return findings, files, nil
}

func workflowFile(entry os.DirEntry) bool {
	ext := filepath.Ext(entry.Name())
	return !entry.IsDir() && (ext == ".yml" || ext == ".yaml")
}

func mapping(node yaml.Node) ([]namedNode, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at line %d", node.Line)
	}
	if len(node.Content) > maxEntries*2 {
		return nil, fmt.Errorf("mapping at line %d exceeds %d entries", node.Line, maxEntries)
	}
	var result []namedNode
	for i := 0; i+1 < len(node.Content) && i < maxEntries*2; i += 2 {
		result = append(result, namedNode{node.Content[i].Value, node.Content[i+1]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	for i := 1; i < len(result) && i < maxEntries; i++ {
		if result[i-1].name == result[i].name {
			return nil, fmt.Errorf("duplicate key %q at line %d", result[i].name, node.Line)
		}
	}
	return result, nil
}

func mainOnly(on yaml.Node) bool {
	if on.Kind == yaml.ScalarNode {
		return on.Value == "schedule"
	}
	if on.Kind == yaml.SequenceNode {
		return scheduleSequence(on)
	}
	triggers, err := mapping(on)
	if err != nil || len(triggers) == 0 {
		return false
	}
	for i := 0; i < len(triggers) && i < maxEntries; i++ {
		if !mainTrigger(triggers[i]) {
			return false
		}
	}
	return true
}

func mainTrigger(trigger namedNode) bool {
	switch trigger.name {
	case "schedule":
		return true
	case "push":
		return mainPush(*trigger.node)
	default:
		return false
	}
}

func scheduleSequence(on yaml.Node) bool {
	if len(on.Content) == 0 || len(on.Content) > maxEntries {
		return false
	}
	for i := 0; i < len(on.Content) && i < maxEntries; i++ {
		if on.Content[i].Kind != yaml.ScalarNode || on.Content[i].Value != "schedule" {
			return false
		}
	}
	return true
}

func mainPush(push yaml.Node) bool {
	fields, err := mapping(push)
	if err != nil {
		return false
	}
	branches := false
	for i := 0; i < len(fields) && i < maxEntries; i++ {
		f := fields[i]
		switch f.name {
		case "tags", "tags-ignore", "branches-ignore":
			return false
		case "branches":
			branches = mainBranch(*f.node)
		}
	}
	return branches
}

func mainBranch(node yaml.Node) bool {
	if node.Kind != yaml.SequenceNode || len(node.Content) != 1 {
		return false
	}
	return node.Content[0].Kind == yaml.ScalarNode && node.Content[0].Value == "main"
}

func checkStep(file, job string, index int, s step, guarded bool) []Finding {
	label := s.Name
	if label == "" {
		label = fmt.Sprintf("%d (%s)", index+1, s.Uses)
	}
	guarded = guarded || mainRefGuard(s.If)
	findings := checkAction(file, job, label, s, guarded)
	if !guarded && strings.Contains(s.Run, "type=gha") {
		findings = append(findings, Finding{file, job, label, "add github.ref == 'refs/heads/main' to the step if"})
	}
	return findings
}

func checkAction(file, job, label string, s step, guarded bool) []Finding {
	if s.Uses == "" {
		return nil
	}
	action, _, _ := strings.Cut(s.Uses, "@")
	behaviour, known := cacheBehaviours[action]
	if !known {
		return []Finding{{file, job, label, fmt.Sprintf("classify %s in cacheBehaviours from its action.yml", action)}}
	}
	if guarded {
		return nil
	}
	switch behaviour.kind {
	case write:
		return []Finding{{file, job, label, "add github.ref == 'refs/heads/main' to the step if"}}
	case inputGated:
		return checkInputs(file, job, label, s.With, behaviour)
	default:
		return nil
	}
}

func checkInputs(file, job, label string, with yaml.Node, b cacheBehaviour) []Finding {
	inputs, err := mapping(with)
	if err != nil {
		return []Finding{{file, job, label, "use a with mapping and set cache inputs explicitly: " + err.Error()}}
	}
	values := make(map[string]*yaml.Node, len(inputs))
	for i := 0; i < len(inputs) && i < maxEntries; i++ {
		values[inputs[i].name] = inputs[i].node
	}
	var findings []Finding
	if b.disabled != "" && !disabled(values[b.disabled]) {
		findings = append(findings, Finding{file, job, label, "set " + b.disabled + ": false"})
	}
	if b.empty != "" && !absentOrEmpty(values[b.empty]) {
		findings = append(findings, Finding{file, job, label, "remove " + b.empty + " or set " + b.empty + ": ''"})
	}
	return findings
}

func absentOrEmpty(node *yaml.Node) bool {
	return node == nil || (node.Kind == yaml.ScalarNode && node.Tag == "!!str" && node.Value == "")
}

func disabled(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode {
		return false
	}
	if node.Tag == "!!bool" {
		return strings.EqualFold(node.Value, "false")
	}
	return node.Tag == "!!str" && (node.Value == "false" || mainRefGuard(node.Value))
}

func mainRefGuard(expr string) bool {
	if len(expr) > maxExpression {
		return false
	}
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "${{") && strings.HasSuffix(expr, "}}") {
		expr = strings.TrimSpace(expr[3 : len(expr)-2])
	}
	expr = strings.Join(strings.Fields(expr), " ")
	if strings.Contains(expr, "||") || strings.Contains(expr, "!(") || strings.Contains(expr, "! (") {
		return false
	}
	expr = stripParentheses(expr)
	terms := guardTerms(expr)
	for i := 0; i < len(terms) && i < maxExpression; i++ {
		if stripParentheses(terms[i]) == mainRef {
			return true
		}
	}
	return false
}

// expressionState tracks quotes so parentheses inside string literals do not
// turn a nested comparison into a top-level guard.
type expressionState struct {
	depth int
	quote byte
}

func (s *expressionState) consume(c byte) {
	if s.quote != 0 {
		if c == s.quote {
			s.quote = 0
		}
		return
	}
	switch c {
	case '\'', '"':
		s.quote = c
	case '(':
		s.depth++
	case ')':
		s.depth--
	}
}

func (s expressionState) balanced() bool { return s.depth == 0 && s.quote == 0 }

func topConjunction(expr string, i int, state expressionState) bool {
	return state.balanced() && i+1 < len(expr) && expr[i:i+2] == "&&"
}

func guardTerms(expr string) []string {
	var state expressionState
	start := 0
	var terms []string
	for i := 0; i < len(expr) && i < maxExpression; i++ {
		state.consume(expr[i])
		if state.depth < 0 {
			return nil
		}
		if topConjunction(expr, i, state) {
			terms = append(terms, expr[start:i])
			i++
			start = i + 1
		}
	}
	if !state.balanced() {
		return nil
	}
	return append(terms, expr[start:])
}

func stripParentheses(expr string) string {
	expr = strings.TrimSpace(expr)
	for i := 0; i < maxParentheses; i++ {
		if !outerParentheses(expr) {
			break
		}
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if outerParentheses(expr) {
		return ""
	}
	return expr
}

func outerParentheses(expr string) bool {
	if len(expr) < 2 || expr[0] != '(' || expr[len(expr)-1] != ')' {
		return false
	}
	var state expressionState
	for i := 0; i < len(expr) && i < maxExpression; i++ {
		state.consume(expr[i])
		if state.balanced() && i != len(expr)-1 {
			return false
		}
	}
	return state.balanced()
}
