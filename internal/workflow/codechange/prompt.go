package codechange

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/memory"
)

var errPromptTooLarge = errors.New("agent prompt exceeds configured limit")

type promptInput struct {
	Task    string
	BaseSHA string
	Profile string
	Facts   map[string]string
	Memory  []memory.Memory
}

func buildPrompt(input promptInput, limit int) (string, error) {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Task\n%s\n\nRepository\nBase SHA: %s\nProfile: %s\n\n", input.Task, input.BaseSHA, input.Profile)
	prompt.WriteString("Constraints\n")
	prompt.WriteString("- Work only inside the current workspace.\n")
	prompt.WriteString("- Do not publish, push, merge, tag, or read external credentials.\n")
	prompt.WriteString("- Leave all intended file changes in the workspace.\n\n")
	prompt.WriteString("Preflight")

	keys := make([]string, 0, len(input.Facts))
	for key := range input.Facts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&prompt, "\n- %s: %s", key, input.Facts[key])
	}
	prompt.WriteString("\n\nRelevant memory")
	for _, item := range input.Memory {
		fmt.Fprintf(&prompt, "\n- [%s] %s", item.ID, item.Content)
	}

	result := prompt.String()
	if limit <= 0 || len(result) > limit {
		return "", errPromptTooLarge
	}
	return result, nil
}
