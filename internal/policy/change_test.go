package policy_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/policy"
)

func TestChangePolicyDeniesUnsafePathsModesAndCredentialFiles(t *testing.T) {
	evaluator, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change artifact.Change
		ruleID string
	}{
		{name: "parent path", change: artifact.Change{Path: "../escape", Status: "A", NewMode: "100644"}, ruleID: "path-unsafe"},
		{name: "absolute path", change: artifact.Change{Path: "/escape", Status: "A", NewMode: "100644"}, ruleID: "path-unsafe"},
		{name: "gitlink", change: artifact.Change{Path: "module", Status: "M", NewMode: "160000"}, ruleID: "mode-gitlink"},
		{name: "dot env", change: artifact.Change{Path: ".env", Status: "A", NewMode: "100644"}, ruleID: "path-sensitive"},
		{name: "ssh key", change: artifact.Change{Path: "keys/id_rsa", Status: "A", NewMode: "100600"}, ruleID: "path-sensitive"},
		{name: "pem", change: artifact.Change{Path: "keys/server.pem", Status: "A", NewMode: "100600"}, ruleID: "path-sensitive"},
		{name: "credentials", change: artifact.Change{Path: "credentials.json", Status: "A", NewMode: "100600"}, ruleID: "path-sensitive"},
		{name: "npmrc", change: artifact.Change{Path: ".npmrc", Status: "A", NewMode: "100600"}, ruleID: "path-sensitive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := evaluator.Evaluate(context.Background(), gitcapture.Result{Changes: []artifact.Change{tc.change}})
			if decision.Allowed || !hasRule(decision, tc.ruleID) {
				t.Fatalf("Evaluate() = %#v, want denial with %q", decision, tc.ruleID)
			}
			if len(decision.Findings) == 0 || decision.Findings[0].Path != normalize(tc.change.Path) {
				t.Fatalf("finding paths = %#v, want normalized %q", decision.Findings, normalize(tc.change.Path))
			}
		})
	}
}

func TestChangePolicyDetectsAddedSecretsWithoutLeakingValues(t *testing.T) {
	evaluator, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, added, ruleID string
	}{
		{name: "private key", added: "-----BEGIN PRIVATE KEY-----", ruleID: "secret-private-key"},
		{name: "github token", added: "ghp_abcdefghijklmnopqrstuvwxyz1234567890", ruleID: "secret-github-token"},
		{name: "secret assignment", added: "DATABASE_SECRET=super-secret-value", ruleID: "secret-assignment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch := "diff --git a/src/config.go b/src/config.go\n--- a/src/config.go\n+++ b/src/config.go\n@@ -0,0 +1 @@\n+" + tc.added + "\n"
			decision := evaluator.Evaluate(context.Background(), gitcapture.Result{Patch: []byte(patch)})
			if decision.Allowed || !hasRule(decision, tc.ruleID) {
				t.Fatalf("Evaluate() = %#v, want denial with %q", decision, tc.ruleID)
			}
			if strings.Contains(fmt.Sprint(decision.Findings), "super-secret-value") || strings.Contains(fmt.Sprint(decision.Findings), tc.added) {
				t.Fatalf("policy finding leaked matched value: %#v", decision.Findings)
			}
		})
	}
}

func TestChangePolicyOnlyScansAddedTextAndAllowsOrdinaryPatch(t *testing.T) {
	evaluator, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/main.go b/src/main.go\n" +
		"similarity index 90%\nrename from src/old.go\nrename to src/main.go\n" +
		"--- a/src/old.go\n+++ b/src/main.go\n@@ -1 +1 @@\n" +
		"-DATABASE_SECRET=super-secret-value\n+fmt.Println(\"safe\")\n" +
		"GIT binary patch\nliteral 4\nabc\x00\n"
	decision := evaluator.Evaluate(context.Background(), gitcapture.Result{
		Patch:   []byte(patch),
		Changes: []artifact.Change{{Path: "src/main.go", OldPath: "src/old.go", Status: "R", OldMode: "100644", NewMode: "100644"}},
	})
	if !decision.Allowed || len(decision.Findings) != 0 {
		t.Fatalf("Evaluate() = %#v, want ordinary change allowed", decision)
	}
}

func TestChangePolicyFindingsAreSorted(t *testing.T) {
	evaluator, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	decision := evaluator.Evaluate(context.Background(), gitcapture.Result{Changes: []artifact.Change{
		{Path: "z.pem", Status: "A", NewMode: "100600"},
		{Path: ".env", Status: "A", NewMode: "100600"},
	}})
	if decision.Allowed || !sort.SliceIsSorted(decision.Findings, func(i, j int) bool {
		left, right := decision.Findings[i], decision.Findings[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.RuleID != right.RuleID {
			return left.RuleID < right.RuleID
		}
		return left.Line < right.Line
	}) {
		t.Fatalf("findings = %#v, want sorted denial", decision.Findings)
	}
}

func hasRule(decision policy.Decision, ruleID string) bool {
	for _, finding := range decision.Findings {
		if finding.RuleID == ruleID {
			return true
		}
	}
	return false
}

func normalize(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
}
