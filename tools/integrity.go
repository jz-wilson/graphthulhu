package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skridlevsky/graphthulhu/backend"
	"github.com/skridlevsky/graphthulhu/graph"
	"github.com/skridlevsky/graphthulhu/types"
)

// Integrity implements graph integrity checking tools.
type Integrity struct {
	client backend.Backend
	cache  *graph.Cache
}

// NewIntegrity creates a new Integrity tool handler.
func NewIntegrity(c backend.Backend) *Integrity {
	return &Integrity{
		client: c,
		cache:  graph.NewCache(c, 30*time.Second),
	}
}

// ListBrokenLinks scans all wikilinks and reports those pointing to non-existent pages.
func (i *Integrity) ListBrokenLinks(ctx context.Context, req *mcp.CallToolRequest, input types.ListBrokenLinksInput) (*mcp.CallToolResult, any, error) {
	g, err := i.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	type brokenLink struct {
		FromPage   string `json:"fromPage"`
		Link       string `json:"link"`
		Suggestion string `json:"suggestion,omitempty"`
	}

	var broken []brokenLink
	for fromLower, targets := range g.Forward {
		fromPage := fromLower
		if p, ok := g.Pages[fromLower]; ok && p.Name != "" {
			fromPage = p.Name
		}
		for target := range targets {
			if _, exists := g.Pages[strings.ToLower(target)]; !exists {
				broken = append(broken, brokenLink{
					FromPage:   fromPage,
					Link:       target,
					Suggestion: suggestPage(target, g.Pages),
				})
			}
		}
	}
	sort.Slice(broken, func(i, j int) bool {
		if broken[i].FromPage != broken[j].FromPage {
			return broken[i].FromPage < broken[j].FromPage
		}
		return broken[i].Link < broken[j].Link
	})

	res, err := jsonTextResult(map[string]any{
		"total":  len(broken),
		"broken": broken,
	})
	return res, nil, err
}

// suggestPage finds the closest existing page name using longest common substring.
func suggestPage(target string, pages map[string]types.PageEntity) string {
	targetLower := strings.ToLower(target)
	best := ""
	bestScore := 0
	for lowerName, page := range pages {
		score := longestCommonSubstring(targetLower, lowerName)
		if score > bestScore && score > len(targetLower)/3 {
			bestScore = score
			best = page.Name
		}
	}
	return best
}

func longestCommonSubstring(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	best := 0
	dp := make([]int, len(b))
	for _, ca := range a {
		prev := 0
		for j, cb := range b {
			cur := 0
			if ca == cb {
				cur = prev + 1
				if cur > best {
					best = cur
				}
			}
			prev = dp[j]
			dp[j] = cur
		}
	}
	return best
}
