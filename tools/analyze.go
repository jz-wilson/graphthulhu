package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skridlevsky/graphthulhu/backend"
	"github.com/skridlevsky/graphthulhu/graph"
	"github.com/skridlevsky/graphthulhu/types"
)

// isNumericPageName returns true if the page name consists only of
// digits and common stray characters like parentheses, backticks, etc.
// These are typically artifacts from Logseq block references, not real pages.
func isNumericPageName(name string) bool {
	cleaned := strings.TrimRight(name, ")`,. ")
	if cleaned == "" {
		return true
	}
	for _, r := range cleaned {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// Analyze implements graph analysis MCP tools.
type Analyze struct {
	client backend.Backend
	cache  *graph.Cache
}

// NewAnalyze creates a new Analyze tool handler with a 30-second graph cache.
func NewAnalyze(c backend.Backend) *Analyze {
	return &Analyze{
		client: c,
		cache:  graph.NewCache(c, 30*time.Second),
	}
}

// GraphOverview returns global graph statistics.
func (a *Analyze) GraphOverview(ctx context.Context, req *mcp.CallToolRequest, input types.GraphOverviewInput) (*mcp.CallToolResult, any, error) {
	g, err := a.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	stats := g.Overview()

	res, err := jsonTextResult(stats)
	return res, nil, err
}

// FindConnections finds how two pages are connected in the graph.
func (a *Analyze) FindConnections(ctx context.Context, req *mcp.CallToolRequest, input types.FindConnectionsInput) (*mcp.CallToolResult, any, error) {
	g, err := a.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	result := g.FindConnections(input.From, input.To, input.MaxDepth)

	if !result.DirectlyLinked && len(result.Paths) == 0 && len(result.SharedConnections) == 0 {
		return textResult(fmt.Sprintf("No connections found between '%s' and '%s'.", input.From, input.To)), nil, nil
	}

	res, err := jsonTextResult(result)
	return res, nil, err
}

// KnowledgeGaps finds sparse areas in the knowledge graph.
func (a *Analyze) KnowledgeGaps(ctx context.Context, req *mcp.CallToolRequest, input types.KnowledgeGapsInput) (*mcp.CallToolResult, any, error) {
	g, err := a.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	gaps := g.KnowledgeGaps()

	// Apply optional filters to orphan pages.
	if input.MinBlockCount > 0 || input.ExcludeNumeric {
		filtered := gaps.OrphanPages[:0]
		for _, name := range gaps.OrphanPages {
			if input.MinBlockCount > 0 {
				key := strings.ToLower(name)
				if g.BlockCounts[key] < input.MinBlockCount {
					continue
				}
			}
			if input.ExcludeNumeric && isNumericPageName(name) {
				continue
			}
			filtered = append(filtered, name)
		}
		gaps.OrphanPages = filtered
	}

	res, err := jsonTextResult(gaps)
	return res, nil, err
}

// ListOrphans returns the actual orphan page names (not just a count).
func (a *Analyze) ListOrphans(ctx context.Context, req *mcp.CallToolRequest, input types.ListOrphansInput) (*mcp.CallToolResult, any, error) {
	g, err := a.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	gaps := g.KnowledgeGaps()

	limit := input.Limit
	if limit <= 0 {
		limit = 50
	}

	total := len(gaps.OrphanPages)
	var filtered []map[string]any

	for _, name := range gaps.OrphanPages {
		key := strings.ToLower(name)
		blockCount := g.BlockCounts[key]

		if input.MinBlockCount > 0 && blockCount < input.MinBlockCount {
			continue
		}
		if input.ExcludeNumeric && isNumericPageName(name) {
			continue
		}

		hasProps := false
		if p, ok := g.Pages[key]; ok && len(p.Properties) > 0 {
			hasProps = true
		}

		filtered = append(filtered, map[string]any{
			"name":          name,
			"blockCount":    blockCount,
			"hasProperties": hasProps,
		})

		if len(filtered) >= limit {
			break
		}
	}

	res, err := jsonTextResult(map[string]any{
		"total":    total,
		"returned": len(filtered),
		"orphans":  filtered,
	})
	return res, nil, err
}

// TopicClusters finds community clusters in the knowledge graph.
func (a *Analyze) TopicClusters(ctx context.Context, req *mcp.CallToolRequest, input types.TopicClustersInput) (*mcp.CallToolResult, any, error) {
	g, err := a.cache.Get(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build graph: %v", err)), nil, nil
	}

	clusters := g.TopicClusters()

	if len(clusters) == 0 {
		return textResult("No topic clusters found — the graph may be too sparse or disconnected."), nil, nil
	}

	res, err := jsonTextResult(map[string]any{
		"clusterCount": len(clusters),
		"clusters":     clusters,
	})
	return res, nil, err
}

// PageQuality audits a single page for structural issues such as duplicate H2/H3 headings.
func (a *Analyze) PageQuality(ctx context.Context, req *mcp.CallToolRequest, input types.PageQualityInput) (*mcp.CallToolResult, any, error) {
	blocks, err := a.client.GetPageBlocksTree(ctx, input.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("page not found: %v", err)), nil, nil
	}

	headingCounts := make(map[string]int)
	var collectHeadings func([]types.BlockEntity)
	collectHeadings = func(bs []types.BlockEntity) {
		for _, b := range bs {
			content := strings.TrimSpace(b.Content)
			if strings.HasPrefix(content, "## ") || strings.HasPrefix(content, "### ") {
				h := strings.ToLower(strings.TrimLeft(content, "# "))
				headingCounts[h]++
			}
			if len(b.Children) > 0 {
				collectHeadings(b.Children)
			}
		}
	}
	collectHeadings(blocks)

	type dupHeading struct {
		Heading string `json:"heading"`
		Count   int    `json:"count"`
	}
	var dups []dupHeading
	for h, count := range headingCounts {
		if count > 1 {
			dups = append(dups, dupHeading{Heading: h, Count: count})
		}
	}
	sort.Slice(dups, func(i, j int) bool {
		return dups[i].Heading < dups[j].Heading
	})

	res, err := jsonTextResult(map[string]any{
		"page":              input.Name,
		"healthy":           len(dups) == 0,
		"duplicateHeadings": dups,
	})
	return res, nil, err
}

// ListStalePages ranks pages by staleness (age of updated property).
func (a *Analyze) ListStalePages(ctx context.Context, req *mcp.CallToolRequest, input types.ListStalePagesInput) (*mcp.CallToolResult, any, error) {
	pages, err := a.client.GetAllPages(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to list pages: %v", err)), nil, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}

	type stalePage struct {
		Name      string `json:"name"`
		UpdatedAt string `json:"updatedAt"`
		DaysStale int    `json:"daysStale"`
	}

	now := time.Now()
	var stale []stalePage

	for _, p := range pages {
		var updatedStr string
		if u, ok := p.Properties["updated"]; ok && u != nil {
			updatedStr = fmt.Sprintf("%v", u)
		}

		days := 9999
		displayStr := "unknown"

		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04"} {
			if t, err2 := time.Parse(layout, updatedStr); err2 == nil {
				days = int(now.Sub(t).Hours() / 24)
				displayStr = t.Format("2006-01-02")
				break
			}
		}

		stale = append(stale, stalePage{
			Name:      p.Name,
			UpdatedAt: displayStr,
			DaysStale: days,
		})
	}

	sort.Slice(stale, func(i, j int) bool {
		return stale[i].DaysStale > stale[j].DaysStale
	})
	if len(stale) > limit {
		stale = stale[:limit]
	}

	res, err := jsonTextResult(map[string]any{
		"total": len(pages),
		"pages": stale,
	})
	return res, nil, err
}
