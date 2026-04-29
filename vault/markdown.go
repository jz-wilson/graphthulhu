package vault

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/skridlevsky/graphthulhu/types"
)

// parseMarkdownBlocks parses markdown body (frontmatter already stripped) into a
// block tree compatible with types.BlockEntity. Headings create hierarchically
// nested blocks based on level (H1 > H2 > H3, etc). Content between headings
// is included in the preceding heading block's Content.
//
// filepath is used as a seed for deterministic block UUID generation.
func parseMarkdownBlocks(filepath, body string) []types.BlockEntity {
	if strings.TrimSpace(body) == "" {
		return nil
	}

	lines := strings.Split(body, "\n")

	// Collect sections: each heading or standalone UUID comment starts a new section.
	type section struct {
		level      int    // 0 = pre-heading, 1-6 = H1-H6
		startLine  int    // line number in the original file (for UUID seed)
		lines      []string
		taggedUUID string // non-empty for blocks whose UUID was a standalone comment line
	}

	var sections []section
	current := section{level: 0, startLine: 0}

	for i, line := range lines {
		if lvl := headingLevel(line); lvl > 0 {
			// Save accumulated section.
			if len(current.lines) > 0 || current.level > 0 || current.taggedUUID != "" {
				sections = append(sections, current)
			}
			current = section{level: lvl, startLine: i, lines: []string{line}}
		} else {
			// At root level (no heading seen yet), a standalone UUID comment starts a new
			// tagged section. Inside heading sections (level > 0), UUID comments stay in
			// the heading's lines so the child-detection logic can handle them (Logseq format).
			uuid, clean := extractUUID(line)
			if current.level == 0 && uuid != "" && strings.TrimSpace(clean) == "" {
				if len(current.lines) > 0 || current.taggedUUID != "" {
					sections = append(sections, current)
				}
				current = section{level: 0, startLine: i, taggedUUID: uuid}
			} else {
				current.lines = append(current.lines, line)
			}
		}
	}
	// Save final section.
	if len(current.lines) > 0 || current.level > 0 || current.taggedUUID != "" {
		sections = append(sections, current)
	}

	// Build blocks from sections.
	type stackEntry struct {
		block *types.BlockEntity
		level int
	}

	var roots []types.BlockEntity
	var stack []stackEntry

	for _, sec := range sections {
		rawContent := strings.TrimRight(strings.Join(sec.lines, "\n"), "\n ")
		if rawContent == "" && sec.level == 0 {
			// Skip empty root sections (tagged or untagged). Orphaned <!-- id: --> comment
			// lines without content produce empty tagged sections; skipping them deduplicates
			// cases where the same UUID appears twice with no content on the first occurrence.
			continue
		}

		var blockUUID, cleanContent string
		var inlineChildren []types.BlockEntity

		if sec.taggedUUID != "" {
			// UUID was embedded as a standalone comment line preceding the content.
			blockUUID = sec.taggedUUID
			cleanContent = rawContent
		} else if sec.level > 0 {
			// Heading section: extract UUID from heading line only, then scan
			// remaining lines for Logseq-authored child blocks.
			// Logseq writes children as a UUID-only comment line followed by the
			// content line (prefix format): "<!-- id: UUID -->\ncontent".
			headingUUID, headingClean := extractUUID(sec.lines[0])
			if headingUUID == "" {
				headingUUID = deterministicUUID(filepath, sec.startLine)
				headingClean = sec.lines[0]
			}
			blockUUID = headingUUID

			var pendingChildUUID string
			hasChildUUIDs := false
			for _, line := range sec.lines[1:] {
				childUUID, childClean := extractUUID(line)
				if childUUID != "" && strings.TrimSpace(childClean) == "" {
					// UUID-only line: next content line belongs to this child.
					pendingChildUUID = childUUID
					hasChildUUIDs = true
				} else if pendingChildUUID != "" {
					// Content line paired with the preceding UUID-only line.
					inlineChildren = append(inlineChildren, types.BlockEntity{
						UUID:    pendingChildUUID,
						Content: line,
					})
					pendingChildUUID = ""
				}
				// Non-UUID lines without a pending UUID are section prose;
				// they stay in the parent block content below.
			}

			if hasChildUUIDs {
				// Parent holds only the heading; children own the paragraphs/rows.
				// This makes UpdateBlock on the heading match just that one line.
				cleanContent = headingClean
			} else {
				cleanContent = strings.TrimRight(strings.Join(append([]string{headingClean}, sec.lines[1:]...), "\n"), "\n ")
			}
		} else {
			blockUUID, cleanContent = getOrCreateUUID(filepath, sec.startLine, rawContent)
		}

		block := types.BlockEntity{
			UUID:     blockUUID,
			Content:  cleanContent,
			Children: inlineChildren,
		}

		if sec.level == 0 {
			// Pre-heading content is always a root block.
			roots = append(roots, block)
			continue
		}

		// Pop stack until we find a parent with lower heading level.
		for len(stack) > 0 && stack[len(stack)-1].level >= sec.level {
			stack = stack[:len(stack)-1]
		}

		if len(stack) == 0 {
			// Top-level heading block.
			roots = append(roots, block)
			stack = append(stack, stackEntry{block: &roots[len(roots)-1], level: sec.level})
		} else {
			// Child of the block at top of stack.
			parent := stack[len(stack)-1].block
			parent.Children = append(parent.Children, block)
			child := &parent.Children[len(parent.Children)-1]
			stack = append(stack, stackEntry{block: child, level: sec.level})
		}
	}

	return roots
}

// headingLevel returns the heading level (1-6) for a markdown heading line,
// or 0 if the line is not a heading.
func headingLevel(line string) int {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0
	}

	level := 0
	for _, ch := range trimmed {
		if ch == '#' {
			level++
		} else {
			break
		}
	}

	if level > 6 || level == 0 {
		return 0
	}

	// Must be followed by a space or be just hashes (e.g. "## " or "##").
	rest := trimmed[level:]
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return 0
	}

	return level
}

// deterministicUUID generates a stable UUID from a filepath and line number.
// Same content at the same location always produces the same UUID.
// Used as fallback for backward compatibility with files that don't have embedded UUIDs.
func deterministicUUID(filepath string, lineNumber int) string {
	seed := fmt.Sprintf("%s:%d", filepath, lineNumber)
	h := sha256.Sum256([]byte(seed))
	hex := fmt.Sprintf("%x", h)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex[:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32])
}

// uuidCommentPattern matches HTML comments containing UUIDs: <!-- id: UUID -->
var uuidCommentPattern = regexp.MustCompile(`<!--\s*id:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\s*-->`)

// extractUUID attempts to extract a UUID from an HTML comment in the content.
// Returns the UUID and the content with the comment stripped.
// If no UUID comment is found, returns empty string and original content.
func extractUUID(content string) (string, string) {
	matches := uuidCommentPattern.FindStringSubmatch(content)
	if len(matches) >= 2 {
		uuid := matches[1]
		// Remove the HTML comment from content
		cleanContent := uuidCommentPattern.ReplaceAllString(content, "")
		cleanContent = strings.TrimSpace(cleanContent)
		return uuid, cleanContent
	}
	return "", content
}

// embedUUID adds a UUID HTML comment to the content.
// For headings, it adds at the end of the heading line.
// For other content, it adds as a standalone line at the beginning.
func embedUUID(content, blockUUID string) string {
	comment := fmt.Sprintf("<!-- id: %s -->", blockUUID)

	// If content starts with a heading, add comment at end of heading line
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && headingLevel(lines[0]) > 0 {
		lines[0] = strings.TrimSpace(lines[0]) + " " + comment
		return strings.Join(lines, "\n")
	}

	// Otherwise add as standalone line at beginning
	return comment + "\n" + content
}

// getOrCreateUUID extracts UUID from content, or generates a new one.
// Returns the UUID and cleaned content (with UUID comment removed).
func getOrCreateUUID(filepath string, lineNumber int, content string) (string, string) {
	// Try to extract existing UUID
	blockUUID, cleanContent := extractUUID(content)
	if blockUUID != "" {
		return blockUUID, cleanContent
	}

	// No embedded UUID found, use deterministic UUID as fallback
	// This provides backward compatibility
	return deterministicUUID(filepath, lineNumber), cleanContent
}

// datePattern matches YYYY-MM-DD date strings used in Obsidian journal page names.
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// isDateString returns true if the string is a YYYY-MM-DD date.
func isDateString(s string) bool {
	return datePattern.MatchString(s)
}
