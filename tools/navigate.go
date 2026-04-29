package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skridlevsky/graphthulhu/backend"
	"github.com/skridlevsky/graphthulhu/parser"
	"github.com/skridlevsky/graphthulhu/types"
)

// Navigate implements navigation MCP tools.
type Navigate struct {
	client backend.Backend
}

// NewNavigate creates a new Navigate tool handler.
func NewNavigate(c backend.Backend) *Navigate {
	return &Navigate{client: c}
}

// GetPage retrieves a page with its full recursive block tree and parsed content.
func (n *Navigate) GetPage(ctx context.Context, req *mcp.CallToolRequest, input types.GetPageInput) (*mcp.CallToolResult, any, error) {
	page, err := n.client.GetPage(ctx, input.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("page not found: %s — %v", input.Name, err)), nil, nil
	}
	if page == nil {
		return errorResult(fmt.Sprintf("page not found: %s", input.Name)), nil, nil
	}

	blocks, err := n.client.GetPageBlocksTree(ctx, input.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to get blocks for %s: %v", input.Name, err)), nil, nil
	}

	depth := input.Depth
	if depth == 0 {
		depth = -1 // unlimited by default
	}

	enrichedBlocks := enrichBlockTree(blocks, depth, 0, make(map[string]bool))

	totalBlocks := countBlocks(enrichedBlocks)
	truncated := false

	if input.MaxBlocks > 0 && totalBlocks > input.MaxBlocks {
		enrichedBlocks = truncateBlockTree(enrichedBlocks, input.MaxBlocks)
		truncated = true
	}

	if input.ExcludeBoilerplate {
		enrichedBlocks = filterBoilerplate(enrichedBlocks, boilerplatePrefixes())
		// Also filter raw blocks used by compact mode below.
		blocks = filterBoilerplateRaw(blocks, boilerplatePrefixes())
	}

	outgoing := collectOutgoingLinks(enrichedBlocks)

	result := map[string]any{
		"page":          page,
		"outgoingLinks": outgoing,
		"linkCount":     len(outgoing),
	}

	if input.IncludeBacklinks {
		backlinks := n.getBacklinks(ctx, input.Name)
		result["backlinks"] = backlinks
		result["linkCount"] = len(outgoing) + len(backlinks)
	}

	if input.Compact {
		// Compact mode: blocks as plain strings with UUIDs.
		// Use raw blocks (not enriched) to avoid exponential re-enrichment.
		compactBlocks := flattenBlocksCompact(blocks)
		if input.MaxBlocks > 0 && len(compactBlocks) > input.MaxBlocks {
			compactBlocks = compactBlocks[:input.MaxBlocks]
			truncated = true
		}
		result["blocks"] = compactBlocks
		result["blockCount"] = len(compactBlocks)
	} else {
		result["blocks"] = enrichedBlocks
		result["blockCount"] = countBlocks(enrichedBlocks)
	}

	if truncated {
		result["truncated"] = true
		result["totalBlocks"] = totalBlocks
	}

	res, err := jsonTextResult(result)
	return res, nil, err
}

// GetPages retrieves multiple pages in a single call.
func (n *Navigate) GetPages(ctx context.Context, req *mcp.CallToolRequest, input types.GetPagesInput) (*mcp.CallToolResult, any, error) {
	results := make(map[string]any, len(input.Names))

	for _, name := range input.Names {
		page, err := n.client.GetPage(ctx, name)
		if err != nil || page == nil {
			results[name] = map[string]any{"error": fmt.Sprintf("page not found: %s", name)}
			continue
		}

		blocks, err := n.client.GetPageBlocksTree(ctx, name)
		if err != nil {
			results[name] = map[string]any{"error": fmt.Sprintf("failed to get blocks: %v", err)}
			continue
		}

		if input.Compact {
			rawBlocks := blocks
			if input.ExcludeBoilerplate {
				rawBlocks = filterBoilerplateRaw(rawBlocks, boilerplatePrefixes())
			}
			compactBlocks := flattenBlocksCompact(rawBlocks)
			results[name] = map[string]any{
				"page":       page,
				"blocks":     compactBlocks,
				"blockCount": len(compactBlocks),
			}
		} else {
			enrichedBlocks := enrichBlockTree(blocks, -1, 0, make(map[string]bool))
			if input.ExcludeBoilerplate {
				enrichedBlocks = filterBoilerplate(enrichedBlocks, boilerplatePrefixes())
			}
			results[name] = map[string]any{
				"page":       page,
				"blocks":     enrichedBlocks,
				"blockCount": countBlocks(enrichedBlocks),
			}
		}
	}

	res, err := jsonTextResult(results)
	return res, nil, err
}

// BatchGetPagesSummary returns frontmatter-only for a list of named pages.
// No block tree is loaded — cuts round-trips when loading cluster metadata.
func (n *Navigate) BatchGetPagesSummary(ctx context.Context, req *mcp.CallToolRequest, input types.BatchGetPagesSummaryInput) (*mcp.CallToolResult, any, error) {
	type PageSummary struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Type        string `json:"type,omitempty"`
		Updated     string `json:"updated,omitempty"`
		Found       bool   `json:"found"`
	}

	summaries := make([]PageSummary, 0, len(input.Pages))

	for _, name := range input.Pages {
		page, err := n.client.GetPage(ctx, name)
		if err != nil || page == nil {
			summaries = append(summaries, PageSummary{Name: name, Found: false})
			continue
		}

		s := PageSummary{Name: name, Found: true}
		if page.Properties != nil {
			if v, ok := page.Properties["description"]; ok {
				s.Description = fmt.Sprintf("%v", v)
			}
			if v, ok := page.Properties["type"]; ok {
				s.Type = fmt.Sprintf("%v", v)
			}
			if v, ok := page.Properties["updated"]; ok {
				s.Updated = fmt.Sprintf("%v", v)
			}
		}
		// Use OriginalName if available (preserves casing)
		if page.OriginalName != "" {
			s.Name = page.OriginalName
		}
		summaries = append(summaries, s)
	}

	res, err := jsonTextResult(summaries)
	return res, nil, err
}

// GetBlock retrieves a block with ancestors, children, and optionally siblings.
func (n *Navigate) GetBlock(ctx context.Context, req *mcp.CallToolRequest, input types.GetBlockInput) (*mcp.CallToolResult, any, error) {
	opts := map[string]any{"includeChildren": true}
	block, err := n.client.GetBlock(ctx, input.UUID, opts)
	if err != nil {
		return errorResult(fmt.Sprintf("block not found: %s — %v", input.UUID, err)), nil, nil
	}
	if block == nil {
		return errorResult(fmt.Sprintf("block not found: %s", input.UUID)), nil, nil
	}

	enriched := enrichBlock(*block)

	if input.IncludeAncestors {
		ancestors, err := n.getAncestors(ctx, input.UUID)
		if err == nil {
			enriched.Ancestors = ancestors
		}
	}

	res, err := jsonTextResult(&enriched)
	return res, nil, err
}

// ListPages lists pages with optional filtering.
func (n *Navigate) ListPages(ctx context.Context, req *mcp.CallToolRequest, input types.ListPagesInput) (*mcp.CallToolResult, any, error) {
	pages, err := n.client.GetAllPages(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to list pages: %v", err)), nil, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 50
	}

	var filtered []types.PageEntity
	for _, p := range pages {
		if p.Name == "" {
			continue
		}
		if input.Namespace != "" {
			if !strings.HasPrefix(strings.ToLower(p.Name), strings.ToLower(input.Namespace)) {
				continue
			}
		}
		if input.HasProperty != "" {
			if p.Properties == nil {
				continue
			}
			if _, ok := p.Properties[input.HasProperty]; !ok {
				continue
			}
		}
		if input.HasTag != "" {
			blocks, err := n.client.GetPageBlocksTree(ctx, p.Name)
			if err != nil {
				continue
			}
			if !pageHasTag(blocks, strings.ToLower(input.HasTag)) {
				continue
			}
		}
		filtered = append(filtered, p)
	}

	sortBy := input.SortBy
	if sortBy == "" {
		sortBy = "name"
	}
	sortPages(filtered, sortBy)

	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	summaries := make([]map[string]any, len(filtered))
	for i, p := range filtered {
		summaries[i] = map[string]any{
			"name":       p.OriginalName,
			"properties": p.Properties,
			"journal":    p.Journal,
		}
		if p.UpdatedAt > 0 {
			summaries[i]["updatedAt"] = p.UpdatedAt
		}
	}

	res, err := jsonTextResult(summaries)
	return res, nil, err
}

// GraphIndex returns frontmatter-only (name, description, type, updated) for all pages.
// Supports optional filtering by type and updated_after date.
func (n *Navigate) GraphIndex(ctx context.Context, req *mcp.CallToolRequest, input types.GraphIndexInput) (*mcp.CallToolResult, any, error) {
	pages, err := n.client.GetAllPages(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to list pages: %v", err)), nil, nil
	}

	type pageEntry struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Type        string `json:"type,omitempty"`
		Updated     string `json:"updated,omitempty"`
	}

	var result []pageEntry
	for _, p := range pages {
		if p.Name == "" {
			continue
		}

		// Extract frontmatter fields from Properties.
		name := p.OriginalName
		if name == "" {
			name = p.Name
		}

		var description, pageType, updated string
		if p.Properties != nil {
			if v, ok := p.Properties["description"]; ok {
				description = fmt.Sprintf("%v", v)
			}
			if v, ok := p.Properties["type"]; ok {
				pageType = fmt.Sprintf("%v", v)
			}
			if v, ok := p.Properties["updated"]; ok {
				updated = fmt.Sprintf("%v", v)
			}
		}

		// Apply type filter.
		if input.Type != "" && !strings.EqualFold(pageType, input.Type) {
			continue
		}

		// Apply updated_after filter.
		if input.UpdatedAfter != "" && updated != "" {
			if updated < input.UpdatedAfter {
				continue
			}
		}

		result = append(result, pageEntry{
			Name:        name,
			Description: description,
			Type:        pageType,
			Updated:     updated,
		})
	}

	// Sort by name for stable output.
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && strings.ToLower(result[j].Name) < strings.ToLower(result[j-1].Name); j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}

	res, err := jsonTextResult(result)
	return res, nil, err
}

// GetLinks returns forward links and backlinks for a page.
func (n *Navigate) GetLinks(ctx context.Context, req *mcp.CallToolRequest, input types.GetLinksInput) (*mcp.CallToolResult, any, error) {
	direction := input.Direction
	if direction == "" {
		direction = "both"
	}

	result := map[string]any{
		"page": input.Name,
	}

	if direction == "forward" || direction == "both" {
		blocks, err := n.client.GetPageBlocksTree(ctx, input.Name)
		if err == nil {
			outgoing := collectAllLinks(blocks)
			result["outgoingLinks"] = outgoing
		}
	}

	if direction == "backward" || direction == "both" {
		backlinks := n.getBacklinks(ctx, input.Name)
		result["backlinks"] = backlinks
	}

	res, err := jsonTextResult(result)
	return res, nil, err
}

// GetReferences finds all blocks referencing a specific block via ((uuid)).
func (n *Navigate) GetReferences(ctx context.Context, req *mcp.CallToolRequest, input types.GetReferencesInput) (*mcp.CallToolResult, any, error) {
	query := fmt.Sprintf(`[:find (pull ?b [:block/uuid :block/content {:block/page [:block/name]}])
		:where
		[?b :block/refs ?ref]
		[?ref :block/uuid #uuid "%s"]]`, input.UUID)

	raw, err := n.client.DatascriptQuery(ctx, query)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to query references: %v", err)), nil, nil
	}

	res, err := jsonRawTextResult(raw)
	return res, nil, err
}

// Traverse finds paths between two pages using BFS on the link graph.
func (n *Navigate) Traverse(ctx context.Context, req *mcp.CallToolRequest, input types.TraverseInput) (*mcp.CallToolResult, any, error) {
	maxHops := input.MaxHops
	if maxHops <= 0 {
		maxHops = 4
	}

	paths := n.bfs(ctx, input.From, input.To, maxHops)

	if len(paths) == 0 {
		return textResult(fmt.Sprintf("No path found between '%s' and '%s' within %d hops.", input.From, input.To, maxHops)), nil, nil
	}

	result := map[string]any{
		"from":       input.From,
		"to":         input.To,
		"pathsFound": len(paths),
		"paths":      paths,
	}

	res, err := jsonTextResult(result)
	return res, nil, err
}

// --- Internal helpers ---

func (n *Navigate) bfs(ctx context.Context, from, to string, maxHops int) [][]string {
	fromLower := strings.ToLower(from)
	toLower := strings.ToLower(to)

	type node struct {
		name string
		path []string
	}

	queue := []node{{name: fromLower, path: []string{from}}}
	visited := map[string]bool{fromLower: true}
	var paths [][]string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if len(current.path) > maxHops+1 {
			break
		}

		blocks, err := n.client.GetPageBlocksTree(ctx, current.name)
		if err != nil {
			continue
		}

		links := collectAllLinks(blocks)
		for _, link := range links {
			linkLower := strings.ToLower(link)
			if linkLower == toLower {
				path := append(append([]string{}, current.path...), link)
				paths = append(paths, path)
				continue
			}
			if !visited[linkLower] && len(current.path) < maxHops {
				visited[linkLower] = true
				newPath := append(append([]string{}, current.path...), link)
				queue = append(queue, node{name: linkLower, path: newPath})
			}
		}
	}

	return paths
}

func (n *Navigate) getBacklinks(ctx context.Context, name string) []types.BackLink {
	raw, err := n.client.GetPageLinkedReferences(ctx, name)
	if err != nil {
		return nil
	}

	var refs [][]json.RawMessage
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil
	}

	var backlinks []types.BackLink
	for _, ref := range refs {
		if len(ref) < 2 {
			continue
		}

		var page types.PageEntity
		if err := json.Unmarshal(ref[0], &page); err != nil {
			continue
		}

		var blocks []types.BlockEntity
		if err := json.Unmarshal(ref[1], &blocks); err != nil {
			continue
		}

		bl := types.BackLink{
			PageName: page.OriginalName,
		}
		if bl.PageName == "" {
			bl.PageName = page.Name
		}
		for _, b := range blocks {
			bl.Blocks = append(bl.Blocks, types.BlockSummary{
				UUID:    b.UUID,
				Content: b.Content,
			})
		}
		backlinks = append(backlinks, bl)
	}

	return backlinks
}

func (n *Navigate) getAncestors(ctx context.Context, uuid string) ([]types.BlockSummary, error) {
	query := fmt.Sprintf(`[:find (pull ?parent [:block/uuid :block/content])
		:where
		[?b :block/uuid #uuid "%s"]
		[?b :block/parent ?parent]]`, uuid)

	raw, err := n.client.DatascriptQuery(ctx, query)
	if err != nil {
		return nil, err
	}

	var results [][]json.RawMessage
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, err
	}

	var ancestors []types.BlockSummary
	for _, r := range results {
		if len(r) == 0 {
			continue
		}
		var block struct {
			UUID    string `json:"uuid"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(r[0], &block); err != nil {
			continue
		}
		if block.UUID != "" {
			ancestors = append(ancestors, types.BlockSummary{
				UUID:    block.UUID,
				Content: block.Content,
			})
		}
	}

	return ancestors, nil
}

// flattenBlocksCompact converts raw blocks to compact string+UUID format.
// Uses BlockEntity directly to avoid re-enriching the tree (which caused exponential blowup).
func flattenBlocksCompact(blocks []types.BlockEntity) []map[string]string {
	var result []map[string]string
	seen := make(map[string]bool)
	flattenCompactRecursive(blocks, 0, &result, seen)
	return result
}

func flattenCompactRecursive(blocks []types.BlockEntity, depth int, result *[]map[string]string, seen map[string]bool) {
	indent := strings.Repeat("  ", depth)
	for _, b := range blocks {
		if b.UUID != "" && seen[b.UUID] {
			continue
		}
		if b.UUID != "" {
			seen[b.UUID] = true
		}
		entry := map[string]string{
			"uuid":    b.UUID,
			"content": indent + b.Content,
		}
		*result = append(*result, entry)
		if len(b.Children) > 0 {
			flattenCompactRecursive(b.Children, depth+1, result, seen)
		}
	}
}

// truncateBlockTree returns at most maxBlocks blocks from the tree (depth-first).
func truncateBlockTree(blocks []types.EnrichedBlock, maxBlocks int) []types.EnrichedBlock {
	var result []types.EnrichedBlock
	remaining := maxBlocks

	for _, b := range blocks {
		if remaining <= 0 {
			break
		}
		remaining--

		if len(b.BlockEntity.Children) > 0 && remaining > 0 {
			// Recursively truncate children
			childEnriched := truncateEnrichedChildren(b.BlockEntity.Children, &remaining)
			b.BlockEntity.Children = childEnriched
		} else {
			b.BlockEntity.Children = nil
		}

		result = append(result, b)
	}

	return result
}

func truncateEnrichedChildren(blocks []types.BlockEntity, remaining *int) []types.BlockEntity {
	var result []types.BlockEntity

	for _, b := range blocks {
		if *remaining <= 0 {
			break
		}
		*remaining--

		if len(b.Children) > 0 && *remaining > 0 {
			b.Children = truncateEnrichedChildren(b.Children, remaining)
		} else {
			b.Children = nil
		}

		result = append(result, b)
	}

	return result
}

func enrichBlockTree(blocks []types.BlockEntity, maxDepth, currentDepth int, seen map[string]bool) []types.EnrichedBlock {
	if maxDepth >= 0 && currentDepth > maxDepth {
		return nil
	}

	enriched := make([]types.EnrichedBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.UUID != "" && seen[b.UUID] {
			continue
		}
		if b.UUID != "" {
			seen[b.UUID] = true
		}
		// Save children before enriching, since enrichBlock copies the whole
		// BlockEntity (including Children). We rebuild Children from the
		// enriched results to avoid duplicating them.
		originalChildren := b.Children
		b.Children = nil
		eb := enrichBlock(b)
		if len(originalChildren) > 0 {
			childEnriched := enrichBlockTree(originalChildren, maxDepth, currentDepth+1, seen)
			for _, ce := range childEnriched {
				eb.BlockEntity.Children = append(eb.BlockEntity.Children, ce.BlockEntity)
			}
		}
		enriched = append(enriched, eb)
	}
	return enriched
}

func enrichBlock(b types.BlockEntity) types.EnrichedBlock {
	return types.EnrichedBlock{
		BlockEntity: b,
		Parsed:      parser.Parse(b.Content),
	}
}

func collectOutgoingLinks(blocks []types.EnrichedBlock) []string {
	seen := make(map[string]bool)
	var links []string
	for _, b := range blocks {
		for _, link := range b.Parsed.Links {
			if !seen[link] {
				links = append(links, link)
				seen[link] = true
			}
		}
	}
	return links
}

func collectAllLinks(blocks []types.BlockEntity) []string {
	seen := make(map[string]bool)
	var links []string
	var walk func([]types.BlockEntity)
	walk = func(bs []types.BlockEntity) {
		for _, b := range bs {
			parsed := parser.Parse(b.Content)
			for _, link := range parsed.Links {
				if !seen[link] {
					links = append(links, link)
					seen[link] = true
				}
			}
			if len(b.Children) > 0 {
				walk(b.Children)
			}
		}
	}
	walk(blocks)
	return links
}

func countBlocks(blocks []types.EnrichedBlock) int {
	count := len(blocks)
	for _, b := range blocks {
		if len(b.BlockEntity.Children) > 0 {
			count += countBlocksRaw(b.BlockEntity.Children)
		}
	}
	return count
}

func countBlocksRaw(blocks []types.BlockEntity) int {
	count := len(blocks)
	for _, b := range blocks {
		if len(b.Children) > 0 {
			count += countBlocksRaw(b.Children)
		}
	}
	return count
}

func sortPages(pages []types.PageEntity, sortBy string) {
	switch sortBy {
	case "modified":
		sortByField(pages, func(p types.PageEntity) int64 { return -p.UpdatedAt })
	case "created":
		sortByField(pages, func(p types.PageEntity) int64 { return -p.CreatedAt })
	default:
		sortByName(pages)
	}
}

// pageHasTag checks if any block in the tree contains the given tag (lowercase).
func pageHasTag(blocks []types.BlockEntity, tagLower string) bool {
	for _, b := range blocks {
		parsed := parser.Parse(b.Content)
		for _, t := range parsed.Tags {
			if strings.ToLower(t) == tagLower {
				return true
			}
		}
		if len(b.Children) > 0 && pageHasTag(b.Children, tagLower) {
			return true
		}
	}
	return false
}

func sortByName(pages []types.PageEntity) {
	for i := 1; i < len(pages); i++ {
		for j := i; j > 0 && strings.ToLower(pages[j].Name) < strings.ToLower(pages[j-1].Name); j-- {
			pages[j], pages[j-1] = pages[j-1], pages[j]
		}
	}
}

func sortByField(pages []types.PageEntity, key func(types.PageEntity) int64) {
	for i := 1; i < len(pages); i++ {
		for j := i; j > 0 && key(pages[j]) < key(pages[j-1]); j-- {
			pages[j], pages[j-1] = pages[j-1], pages[j]
		}
	}
}

// boilerplatePrefixes returns the default prefixes plus any configured via
// GRAPHTHULHU_BOILERPLATE_PREFIXES (comma-separated, case-insensitive).
func boilerplatePrefixes() []string {
	defaults := []string{"[[memory]]", "---"}
	env := os.Getenv("GRAPHTHULHU_BOILERPLATE_PREFIXES")
	if env == "" {
		return defaults
	}
	var extra []string
	for _, p := range strings.Split(env, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			extra = append(extra, strings.ToLower(p))
		}
	}
	return append(defaults, extra...)
}

// filterBoilerplate strips blocks whose Content (lowercased) starts with any
// of the given prefixes. Operates recursively on children.
func filterBoilerplate(blocks []types.EnrichedBlock, prefixes []string) []types.EnrichedBlock {
	var result []types.EnrichedBlock
	for _, b := range blocks {
		contentLower := strings.ToLower(strings.TrimSpace(b.BlockEntity.Content))
		matched := false
		for _, p := range prefixes {
			if strings.HasPrefix(contentLower, p) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if len(b.BlockEntity.Children) > 0 {
			filtered := filterBoilerplateRaw(b.BlockEntity.Children, prefixes)
			b.BlockEntity.Children = filtered
		}
		result = append(result, b)
	}
	return result
}

// filterBoilerplateRaw is the BlockEntity variant used for compact mode and children.
func filterBoilerplateRaw(blocks []types.BlockEntity, prefixes []string) []types.BlockEntity {
	var result []types.BlockEntity
	for _, b := range blocks {
		contentLower := strings.ToLower(strings.TrimSpace(b.Content))
		matched := false
		for _, p := range prefixes {
			if strings.HasPrefix(contentLower, p) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if len(b.Children) > 0 {
			b.Children = filterBoilerplateRaw(b.Children, prefixes)
		}
		result = append(result, b)
	}
	return result
}
