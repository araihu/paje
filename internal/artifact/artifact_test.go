package artifact

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/verification"
)

func TestPreflightBundleSizeBoundsEveryStringAndCollectionOverhead(t *testing.T) {
	const limit = 16 << 10
	large := strings.Repeat("x", limit+1)
	tests := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{name: "manifest run id", mutate: func(bundle *Bundle) { bundle.Manifest.RunID = large }},
		{name: "manifest template name", mutate: func(bundle *Bundle) { bundle.Manifest.Template.Name = large }},
		{name: "manifest repository", mutate: func(bundle *Bundle) { bundle.Manifest.Repository = large }},
		{name: "manifest base sha", mutate: func(bundle *Bundle) { bundle.Manifest.BaseSHA = large }},
		{name: "manifest tree sha", mutate: func(bundle *Bundle) { bundle.Manifest.TreeSHA = large }},
		{name: "change path", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = []Change{{Path: large}} }},
		{name: "change old path", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = []Change{{OldPath: large}} }},
		{name: "change status", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = []Change{{Status: large}} }},
		{name: "change old mode", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = []Change{{OldMode: large}} }},
		{name: "change new mode", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = []Change{{NewMode: large}} }},
		{name: "member name", mutate: func(bundle *Bundle) { bundle.Manifest.Members = []Member{{Name: large}} }},
		{name: "member digest", mutate: func(bundle *Bundle) { bundle.Manifest.Members = []Member{{SHA256: large}} }},
		{name: "verification command name", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Name: large}}}
		}},
		{name: "verification directory", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Directory: large}}}
		}},
		{name: "verification executable", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Executable: large}}}
		}},
		{name: "verification failure class", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{FailureClass: large}}
		}},
		{name: "verification cause code", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{CauseCode: large}}
		}},
		{name: "verification environment key", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Environment: map[string]string{large: ""}}}}
		}},
		{name: "verification environment value", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Environment: map[string]string{"key": large}}}}
		}},
		{name: "empty changes overhead", mutate: func(bundle *Bundle) { bundle.Manifest.Changes = make([]Change, 128) }},
		{name: "empty members overhead", mutate: func(bundle *Bundle) { bundle.Manifest.Members = make([]Member, 256) }},
		{name: "empty memory ids overhead", mutate: func(bundle *Bundle) { bundle.Manifest.MemoryIDs = make([]string, 512) }},
		{name: "empty verification overhead", mutate: func(bundle *Bundle) { bundle.Verification = make([]verification.Result, 64) }},
		{name: "empty verification args overhead", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Args: make([]string, 512)}}}
		}},
		{name: "empty verification environment overhead", mutate: func(bundle *Bundle) {
			bundle.Verification = []verification.Result{{Command: verification.Command{Environment: emptyStringMap(128)}}}
		}},
		{name: "preflight entry overhead", mutate: func(bundle *Bundle) { bundle.Preflight = emptyStringMap(128) }},
		{name: "empty warnings overhead", mutate: func(bundle *Bundle) { bundle.Warnings = make([]string, 512) }},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var bundle Bundle
			test.mutate(&bundle)
			if err := preflightBundleSize(context.Background(), bundle, limit); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("preflightBundleSize() error = %v, want ErrTooLarge", err)
			}
		})
	}
}

func TestWithinLimitUsesOverflowSafeSubtraction(t *testing.T) {
	tests := []struct {
		name             string
		used, added, max int64
		want             bool
	}{
		{name: "exact", used: math.MaxInt64 - 1, added: 1, max: math.MaxInt64, want: true},
		{name: "addition would overflow", used: math.MaxInt64 - 1, added: 2, max: math.MaxInt64, want: false},
		{name: "used already exceeds limit", used: 2, added: 0, max: 1, want: false},
		{name: "negative addition", used: 0, added: -1, max: 1, want: false},
	}
	for _, test := range tests {
		if got := withinLimit(test.used, test.added, test.max); got != test.want {
			t.Errorf("%s: withinLimit(%d, %d, %d) = %t, want %t", test.name, test.used, test.added, test.max, got, test.want)
		}
	}
}

func TestPreflightBundleSizeAccountsForJSONEscaping(t *testing.T) {
	bundle := Bundle{}
	bundle.Manifest.RunID = strings.Repeat("\x00", 2000)
	if err := preflightBundleSize(context.Background(), bundle, 16<<10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("preflightBundleSize() error = %v, want ErrTooLarge", err)
	}
}

func TestNormalizationAndEncodingBoundariesPreserveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := normalizeBundleContext(ctx, Bundle{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("normalizeBundleContext() error = %v, want context.Canceled", err)
	}
	if _, err := encodePayloadsContext(ctx, Bundle{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("encodePayloadsContext() error = %v, want context.Canceled", err)
	}
	if _, err := membersForContext(ctx, map[string][]byte{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("membersForContext() error = %v, want context.Canceled", err)
	}
}

func emptyStringMap(count int) map[string]string {
	values := make(map[string]string, count)
	for index := range count {
		values[string(rune(index+1))] = ""
	}
	return values
}
