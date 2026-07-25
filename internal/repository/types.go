// Package repository defines repository and profile contracts.
package repository

// ModuleExclusion records the explicit reason a discovered module is skipped.
type ModuleExclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
