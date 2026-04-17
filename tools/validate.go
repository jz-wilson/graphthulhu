package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skridlevsky/graphthulhu/backend"
	"github.com/skridlevsky/graphthulhu/types"
)

var updatedDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Validate implements the validate_frontmatter tool.
type Validate struct {
	client backend.Backend
}

// NewValidate creates a new Validate tool handler.
func NewValidate(c backend.Backend) *Validate {
	return &Validate{client: c}
}

var defaultRequiredFields = []string{"name", "description", "type", "updated"}
var defaultAllowedTypes = []string{"feedback", "reference", "user", "project", "people", "infrastructure", "decision", "journal"}

// ValidateFrontmatter reports pages that violate the vault's frontmatter schema.
func (v *Validate) ValidateFrontmatter(ctx context.Context, req *mcp.CallToolRequest, input types.ValidateFrontmatterInput) (*mcp.CallToolResult, any, error) {
	required := defaultRequiredFields
	if len(input.RequiredFields) > 0 {
		required = input.RequiredFields
	}
	allowed := defaultAllowedTypes
	if len(input.AllowedTypes) > 0 {
		allowed = input.AllowedTypes
	}

	allowedSet := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		allowedSet[strings.ToLower(t)] = true
	}

	pages, err := v.client.GetAllPages(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to list pages: %v", err)), nil, nil
	}

	type violation struct {
		Page                 string   `json:"page"`
		MissingFields        []string `json:"missingFields,omitempty"`
		InvalidType          string   `json:"invalidType,omitempty"`
		InvalidUpdatedFormat string   `json:"invalidUpdatedFormat,omitempty"`
	}

	var violations []violation
	for _, p := range pages {
		viol := violation{Page: p.Name}

		for _, f := range required {
			val, ok := p.Properties[f]
			if !ok || val == nil || fmt.Sprintf("%v", val) == "" {
				viol.MissingFields = append(viol.MissingFields, f)
			}
		}

		if t, ok := p.Properties["type"]; ok && t != nil {
			ts := strings.ToLower(fmt.Sprintf("%v", t))
			if ts != "" && !allowedSet[ts] {
				viol.InvalidType = fmt.Sprintf("%v", t)
			}
		}

		if u, ok := p.Properties["updated"]; ok && u != nil {
			us := fmt.Sprintf("%v", u)
			if us != "" && !updatedDateRe.MatchString(us) {
				viol.InvalidUpdatedFormat = us
			}
		}

		if len(viol.MissingFields) > 0 || viol.InvalidType != "" || viol.InvalidUpdatedFormat != "" {
			violations = append(violations, viol)
		}
	}

	res, err := jsonTextResult(map[string]any{
		"total":      len(pages),
		"violations": len(violations),
		"pages":      violations,
	})
	return res, nil, err
}
