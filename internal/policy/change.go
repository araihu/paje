// Package policy evaluates non-overridable safety rules for captured changes.
package policy

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/araihu/paje/internal/artifact/gitcapture"
)

const (
	ruleUnsafePath       = "path-unsafe"
	ruleGitlink          = "mode-gitlink"
	ruleUnsafeMode       = "mode-unsafe"
	ruleSensitivePath    = "path-sensitive"
	rulePrivateKey       = "secret-private-key"
	ruleGitHubToken      = "secret-github-token"
	ruleSecretAssignment = "secret-assignment"
	ruleSecretCapability = "secret-capability"
	ruleCanceled         = "evaluation-canceled"
	policyPath           = ".paje-policy/unknown"
)

var (
	privateKeyPattern  = regexp.MustCompile(`(?i)-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)
	githubTokenPattern = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	assignmentPattern  = regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9_]*_(?:SECRET|TOKEN|PASSWORD|API_KEY)\s*=\s*[^\s#]+`)
)

// Config deliberately has no policy-disable switch. Workspace, when set,
// anchors the lexical path check; empty uses the current filesystem root.
type Config struct {
	Workspace string
}

// Finding describes a policy violation without carrying sensitive match text.
type Finding struct {
	RuleID string `json:"rule_id"`
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
}

// Decision is deterministic so it can be persisted as approval evidence.
type Decision struct {
	Allowed  bool      `json:"allowed"`
	Findings []Finding `json:"findings"`
}

// Evaluator is the workflow port for built-in change policy.
type Evaluator interface {
	Evaluate(context.Context, gitcapture.Result) Decision
}

// SecretDetector is the narrow transient boundary needed to reject exact or
// reversibly encoded leased secret material. Implementations must not expose
// their retained patterns through durable evidence.
type SecretDetector interface {
	Scan([]byte) bool
}

// DetectSecretMaterial applies the transient lease detector to every captured
// byte/string surface. Findings are generic and never contain matched bytes.
func DetectSecretMaterial(ctx context.Context, result gitcapture.Result, detector SecretDetector) Decision {
	if ctx.Err() != nil {
		return canceledDecision()
	}
	if detector == nil {
		return Decision{Allowed: true, Findings: []Finding{}}
	}
	values := [][]byte{result.Patch, []byte(result.TreeSHA)}
	for _, change := range result.Changes {
		values = append(values,
			[]byte(change.Path), []byte(change.OldPath), []byte(change.Status),
			[]byte(change.OldMode), []byte(change.NewMode),
		)
	}
	for _, value := range values {
		if ctx.Err() != nil {
			return canceledDecision()
		}
		if detector.Scan(value) {
			return Decision{Allowed: false, Findings: []Finding{{
				RuleID: ruleSecretCapability, Path: policyPath,
			}}}
		}
	}
	return Decision{Allowed: true, Findings: []Finding{}}
}

// ChangePolicy implements the beta's fixed path, object, and secret rules.
type ChangePolicy struct {
	workspace string
}

var _ Evaluator = (*ChangePolicy)(nil)

// NewChangePolicy constructs the fixed beta policy. It accepts no rule
// overrides; Config exists only to anchor paths when an operator provides one.
func NewChangePolicy(config Config) (*ChangePolicy, error) {
	workspace := config.Workspace
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	return &ChangePolicy{workspace: abs}, nil
}

// Evaluate checks manifest paths and modes before scanning only textual added
// patch lines. Findings deliberately contain no secret values.
func (p *ChangePolicy) Evaluate(ctx context.Context, result gitcapture.Result) Decision {
	findings := make([]Finding, 0)
	if ctx.Err() != nil {
		return canceledDecision()
	}
	for _, change := range result.Changes {
		p.evaluatePath(&findings, change.Path)
		if change.OldPath != "" {
			p.evaluatePath(&findings, change.OldPath)
		}
		oldPath := change.Path
		if change.OldPath != "" {
			oldPath = change.OldPath
		}
		p.evaluateMode(&findings, oldPath, change.OldMode)
		p.evaluateMode(&findings, change.Path, change.NewMode)
		if ctx.Err() != nil {
			return canceledDecision()
		}
	}
	p.evaluatePatch(ctx, &findings, result.Patch)
	if ctx.Err() != nil {
		return canceledDecision()
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].RuleID != findings[j].RuleID {
			return findings[i].RuleID < findings[j].RuleID
		}
		return findings[i].Line < findings[j].Line
	})
	findings = deduplicate(findings)
	return Decision{Allowed: len(findings) == 0, Findings: findings}
}

func (p *ChangePolicy) evaluatePath(findings *[]Finding, value string) {
	normalized, ok := containedPath(p.workspace, value)
	if !ok {
		*findings = append(*findings, Finding{RuleID: ruleUnsafePath, Path: policyPath})
		return
	}
	if sensitivePath(normalized) {
		*findings = append(*findings, Finding{RuleID: ruleSensitivePath, Path: normalized})
	}
}

func (p *ChangePolicy) evaluateMode(findings *[]Finding, value, mode string) {
	if mode == "" || mode == "000000" {
		return
	}
	normalized, ok := containedPath(p.workspace, value)
	if !ok {
		return
	}
	if mode == "160000" {
		*findings = append(*findings, Finding{RuleID: ruleGitlink, Path: normalized})
		return
	}
	if mode != "100644" && mode != "100755" && mode != "120000" {
		*findings = append(*findings, Finding{RuleID: ruleUnsafeMode, Path: normalized})
	}
}

func (p *ChangePolicy) evaluatePatch(ctx context.Context, findings *[]Finding, patch []byte) {
	type patchState uint8
	const (
		outside patchState = iota
		fileHeader
		hunk
		binary
	)
	state := outside
	currentPath := policyPath
	for lineNumber, line := range bytes.Split(patch, []byte("\n")) {
		if ctx.Err() != nil {
			return
		}
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			currentPath = policyPath
			state = fileHeader
			continue
		}
		if state == outside {
			continue
		}
		if bytes.Equal(line, []byte("GIT binary patch")) || bytes.HasPrefix(line, []byte("Binary files ")) {
			state = binary
			continue
		}
		if state != hunk && bytes.HasPrefix(line, []byte("+++ ")) {
			if normalized, ok := p.patchPath(line[4:]); ok {
				currentPath = normalized
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("@@ ")) {
			state = hunk
			continue
		}
		if state != hunk || len(line) < 2 || line[0] != '+' || !utf8.Valid(line[1:]) {
			continue
		}
		text := string(line[1:])
		for _, match := range []struct {
			ruleID string
			re     *regexp.Regexp
		}{
			{rulePrivateKey, privateKeyPattern},
			{ruleGitHubToken, githubTokenPattern},
			{ruleSecretAssignment, assignmentPattern},
		} {
			if match.re.MatchString(text) {
				*findings = append(*findings, Finding{RuleID: match.ruleID, Path: currentPath, Line: lineNumber + 1})
			}
		}
	}
}

func canceledDecision() Decision {
	return Decision{Allowed: false, Findings: []Finding{{RuleID: ruleCanceled, Path: policyPath}}}
}

func deduplicate(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	write := 1
	for read := 1; read < len(findings); read++ {
		if findings[read] == findings[write-1] {
			continue
		}
		findings[write] = findings[read]
		write++
	}
	return findings[:write]
}

func containedPath(workspace, value string) (string, bool) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || filepath.IsAbs(value) {
		return "", false
	}
	candidate := filepath.Join(workspace, filepath.FromSlash(value))
	relative, err := filepath.Rel(workspace, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func sensitivePath(value string) bool {
	base := strings.ToLower(filepath.Base(value))
	return base == ".env" || strings.HasPrefix(base, ".env.") || base == "id_rsa" || strings.HasSuffix(base, ".pem") || base == "credentials.json" || base == ".npmrc"
}

func (p *ChangePolicy) patchPath(value []byte) (string, bool) {
	text := string(value)
	if strings.HasPrefix(text, `"`) {
		decoded, err := strconv.Unquote(text)
		if err != nil {
			return "", false
		}
		text = decoded
	}
	if text == "/dev/null" || !strings.HasPrefix(text, "b/") {
		return "", false
	}
	normalized, ok := containedPath(p.workspace, strings.TrimPrefix(text, "b/"))
	return normalized, ok
}
