package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skridlevsky/graphthulhu/tools"
	"github.com/skridlevsky/graphthulhu/types"
	"github.com/skridlevsky/graphthulhu/vault"
)

// ──────────────────────────────────────────────
// Eval grade scale
// ──────────────────────────────────────────────
//
//   A (95-100)  — No duplicates, journals detected, block counts exact
//   B (80-94)   — Minor issues (e.g. off-by-one block counts)
//   C (60-79)   — One major issue (e.g. journals broken OR duplicates present)
//   D (40-59)   — Multiple issues
//   F (0-39)    — Critical failures
//
// Each test category contributes weighted points to a total score.

const (
	weightDedup    = 40 // Most critical — token cost doubles without it
	weightJournal  = 25 // journal_search/journal_range depend on it
	weightAccuracy = 20 // blockCount accuracy
	weightPerf     = 15 // Response efficiency (tokens saved)
)

type evalResult struct {
	Category string
	Score    int // 0-100 within category
	MaxPts   int // Weight
	Details  string
}

func grade(pct float64) string {
	switch {
	case pct >= 95:
		return "A"
	case pct >= 80:
		return "B"
	case pct >= 60:
		return "C"
	case pct >= 40:
		return "D"
	default:
		return "F"
	}
}

// loadTestVault loads the testdata vault for eval.
func loadTestVault(t *testing.T) *vault.Client {
	t.Helper()
	testdata := filepath.Join("vault", "testdata")
	c := vault.New(testdata)
	if err := c.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	c.BuildBacklinks()
	return c
}

// loadRealVault loads the user's actual 2nd-Brain vault if available.
func loadRealVault(t *testing.T) *vault.Client {
	t.Helper()
	home, _ := os.UserHomeDir()
	vaultPath := filepath.Join(home, "Documents", "2nd-Brain")
	if _, err := os.Stat(vaultPath); os.IsNotExist(err) {
		t.Skip("Real vault not found at ~/Documents/2nd-Brain")
	}
	c := vault.New(vaultPath)
	if err := c.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	c.BuildBacklinks()
	return c
}

// ──────────────────────────────────────────────
// Category 1: Duplicate Block Detection (40pts)
// ──────────────────────────────────────────────

func TestEval_NoDuplicateBlocks_TestVault(t *testing.T) {
	c := loadTestVault(t)
	results := checkDuplicates(t, c)
	reportCategory(t, "Dedup (testdata)", results)
}

func TestEval_NoDuplicateBlocks_RealVault(t *testing.T) {
	c := loadRealVault(t)
	results := checkDuplicates(t, c)
	reportCategory(t, "Dedup (real vault)", results)
}

func checkDuplicates(t *testing.T, c *vault.Client) evalResult {
	t.Helper()
	ctx := context.Background()
	pages, err := c.GetAllPages(ctx)
	if err != nil {
		t.Fatalf("GetAllPages: %v", err)
	}

	totalPages := 0
	pagesWithDups := 0
	totalBlocks := 0
	totalDupBlocks := 0

	for _, p := range pages {
		blocks, err := c.GetPageBlocksTree(ctx, p.Name)
		if err != nil {
			continue
		}
		totalPages++

		uuidCounts := map[string]int{}
		countBlockUUIDs(blocks, uuidCounts)

		pageBlocks := 0
		pageDups := 0
		for _, count := range uuidCounts {
			pageBlocks += count
			if count > 1 {
				pageDups += count - 1
			}
		}
		totalBlocks += pageBlocks
		totalDupBlocks += pageDups
		if pageDups > 0 {
			pagesWithDups++
		}
	}

	score := 100
	if totalBlocks > 0 {
		dupRate := float64(totalDupBlocks) / float64(totalBlocks) * 100
		// Deduct proportionally: 50% dups = score 0
		score = 100 - int(dupRate*2)
		if score < 0 {
			score = 0
		}
	}

	details := fmt.Sprintf(
		"pages=%d, pages_with_dups=%d, total_blocks=%d, dup_blocks=%d, dup_rate=%.1f%%",
		totalPages, pagesWithDups, totalBlocks, totalDupBlocks,
		float64(totalDupBlocks)/float64(max(totalBlocks, 1))*100,
	)

	if pagesWithDups > 0 {
		t.Errorf("FAIL: %d pages have duplicate blocks: %s", pagesWithDups, details)
	}

	return evalResult{
		Category: "Dedup",
		Score:    score,
		MaxPts:   weightDedup,
		Details:  details,
	}
}

func countBlockUUIDs(blocks []types.BlockEntity, counts map[string]int) {
	for _, b := range blocks {
		if b.UUID != "" {
			counts[b.UUID]++
		}
		if len(b.Children) > 0 {
			countBlockUUIDs(b.Children, counts)
		}
	}
}

// ──────────────────────────────────────────────
// Category 2: Journal Detection (25pts)
// ──────────────────────────────────────────────

func TestEval_JournalDetection_TestVault(t *testing.T) {
	c := loadTestVault(t)
	// testdata has daily notes/2026-01-31 and daily notes/2026-02-01
	results := checkJournals(t, c, 2, []string{"daily notes/"})
	reportCategory(t, "Journal Detection (testdata)", results)
}

func TestEval_JournalDetection_RealVault(t *testing.T) {
	c := loadRealVault(t)
	// Real vault uses journals/ namespace
	results := checkJournals(t, c, 1, []string{"journals/"})
	reportCategory(t, "Journal Detection (real vault)", results)
}

func checkJournals(t *testing.T, c *vault.Client, expectedMin int, namespaces []string) evalResult {
	t.Helper()
	ctx := context.Background()
	pages, err := c.GetAllPages(ctx)
	if err != nil {
		t.Fatalf("GetAllPages: %v", err)
	}

	journalCount := 0
	expectedInNs := 0
	detectedInNs := 0

	for _, p := range pages {
		inTargetNs := false
		for _, ns := range namespaces {
			if strings.HasPrefix(p.Name, ns) {
				inTargetNs = true
				break
			}
		}

		if p.Journal {
			journalCount++
			if inTargetNs {
				detectedInNs++
			}
		}
		if inTargetNs {
			expectedInNs++
		}
	}

	score := 0
	if expectedInNs > 0 {
		score = int(float64(detectedInNs) / float64(expectedInNs) * 100)
	} else if journalCount >= expectedMin {
		score = 100
	}

	if journalCount < expectedMin {
		t.Errorf("FAIL: detected %d journals, expected >= %d", journalCount, expectedMin)
	}

	details := fmt.Sprintf(
		"total_journals=%d, expected_min=%d, in_namespace=%d/%d",
		journalCount, expectedMin, detectedInNs, expectedInNs,
	)

	return evalResult{
		Category: "Journal Detection",
		Score:    score,
		MaxPts:   weightJournal,
		Details:  details,
	}
}

// ──────────────────────────────────────────────
// Category 3: Block Count Accuracy (20pts)
// ──────────────────────────────────────────────

func TestEval_BlockCountAccuracy_TestVault(t *testing.T) {
	c := loadTestVault(t)
	results := checkBlockCountAccuracy(t, c)
	reportCategory(t, "Block Count Accuracy (testdata)", results)
}

func TestEval_BlockCountAccuracy_RealVault(t *testing.T) {
	c := loadRealVault(t)
	results := checkBlockCountAccuracy(t, c)
	reportCategory(t, "Block Count Accuracy (real vault)", results)
}

func checkBlockCountAccuracy(t *testing.T, c *vault.Client) evalResult {
	t.Helper()
	ctx := context.Background()

	pages, err := c.GetAllPages(ctx)
	if err != nil {
		t.Fatalf("GetAllPages: %v", err)
	}

	nav := tools.NewNavigate(c)
	totalReported := 0
	totalActual := 0

	for _, p := range pages {
		blocks, err := c.GetPageBlocksTree(ctx, p.Name)
		if err != nil {
			continue
		}

		// Count unique blocks (actual)
		uuids := map[string]bool{}
		countUniqueBlocks(blocks, uuids)
		actual := len(uuids)

		// Get reported count via Navigate tool
		_ = nav // We count from tree directly since tool needs MCP request
		reported := countTotalBlocks(blocks)

		totalReported += reported
		totalActual += actual
	}

	score := 100
	if totalActual > 0 {
		ratio := float64(totalReported) / float64(totalActual)
		if ratio > 1.0 {
			// Inflation penalty: 2x reported = score 0
			overshoot := (ratio - 1.0) * 100
			score = 100 - int(overshoot)
			if score < 0 {
				score = 0
			}
		}
	}

	details := fmt.Sprintf(
		"reported_blocks=%d, unique_blocks=%d, ratio=%.2f",
		totalReported, totalActual,
		float64(totalReported)/float64(max(totalActual, 1)),
	)

	if totalReported != totalActual {
		t.Errorf("WARN: block count mismatch: %s", details)
	}

	return evalResult{
		Category: "Block Count Accuracy",
		Score:    score,
		MaxPts:   weightAccuracy,
		Details:  details,
	}
}

func countUniqueBlocks(blocks []types.BlockEntity, seen map[string]bool) {
	for _, b := range blocks {
		if b.UUID != "" {
			seen[b.UUID] = true
		}
		if len(b.Children) > 0 {
			countUniqueBlocks(b.Children, seen)
		}
	}
}

func countTotalBlocks(blocks []types.BlockEntity) int {
	count := len(blocks)
	for _, b := range blocks {
		count += countTotalBlocks(b.Children)
	}
	return count
}

// ──────────────────────────────────────────────
// Category 4: Efficiency / Token Savings (15pts)
// ──────────────────────────────────────────────

func TestEval_Efficiency_RealVault(t *testing.T) {
	c := loadRealVault(t)
	results := checkEfficiency(t, c)
	reportCategory(t, "Efficiency (real vault)", results)
}

func checkEfficiency(t *testing.T, c *vault.Client) evalResult {
	t.Helper()
	ctx := context.Background()

	pages, err := c.GetAllPages(ctx)
	if err != nil {
		t.Fatalf("GetAllPages: %v", err)
	}

	start := time.Now()
	totalChars := 0
	totalUniqueChars := 0

	for _, p := range pages {
		blocks, err := c.GetPageBlocksTree(ctx, p.Name)
		if err != nil {
			continue
		}

		// Total chars in all blocks (including dups)
		allContent := collectContent(blocks)
		totalChars += len(allContent)

		// Unique content only (deduped by UUID)
		seen := map[string]bool{}
		uniqueContent := collectUniqueContent(blocks, seen)
		totalUniqueChars += len(uniqueContent)
	}
	elapsed := time.Since(start)

	// Perfect efficiency = totalChars == totalUniqueChars
	wasteRatio := 0.0
	if totalChars > 0 {
		wasteRatio = float64(totalChars-totalUniqueChars) / float64(totalChars) * 100
	}

	score := 100 - int(wasteRatio*2) // 50% waste = 0 score
	if score < 0 {
		score = 0
	}

	details := fmt.Sprintf(
		"total_chars=%d, unique_chars=%d, waste=%.1f%%, scan_time=%v, pages=%d",
		totalChars, totalUniqueChars, wasteRatio, elapsed.Round(time.Millisecond), len(pages),
	)

	return evalResult{
		Category: "Efficiency",
		Score:    score,
		MaxPts:   weightPerf,
		Details:  details,
	}
}

func collectContent(blocks []types.BlockEntity) string {
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Content)
		if len(b.Children) > 0 {
			sb.WriteString(collectContent(b.Children))
		}
	}
	return sb.String()
}

func collectUniqueContent(blocks []types.BlockEntity, seen map[string]bool) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.UUID != "" && !seen[b.UUID] {
			seen[b.UUID] = true
			sb.WriteString(b.Content)
		}
		if len(b.Children) > 0 {
			sb.WriteString(collectUniqueContent(b.Children, seen))
		}
	}
	return sb.String()
}

// ──────────────────────────────────────────────
// Full Report
// ──────────────────────────────────────────────

func TestEval_FullReport(t *testing.T) {
	home, _ := os.UserHomeDir()
	vaultPath := filepath.Join(home, "Documents", "2nd-Brain")
	if _, err := os.Stat(vaultPath); os.IsNotExist(err) {
		t.Skip("Real vault not found")
	}

	c := vault.New(vaultPath)
	if err := c.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	c.BuildBacklinks()

	results := []evalResult{
		checkDuplicates(t, c),
		checkJournals(t, c, 1, []string{"journals/"}),
		checkBlockCountAccuracy(t, c),
		checkEfficiency(t, c),
	}

	totalWeighted := 0.0
	totalWeight := 0

	t.Logf("\n")
	t.Logf("╔══════════════════════════════════════════════════════════════╗")
	t.Logf("║              GRAPHTHULHU EVAL REPORT                       ║")
	t.Logf("╠══════════════════════════════════════════════════════════════╣")

	for _, r := range results {
		weighted := float64(r.Score) * float64(r.MaxPts) / 100.0
		totalWeighted += weighted
		totalWeight += r.MaxPts
		g := grade(float64(r.Score))

		t.Logf("║ %-25s  Score: %3d/100  Grade: %s  (×%d)", r.Category, r.Score, g, r.MaxPts)
		t.Logf("║   %s", r.Details)
	}

	finalPct := totalWeighted / float64(totalWeight) * 100
	finalGrade := grade(finalPct)

	t.Logf("╠══════════════════════════════════════════════════════════════╣")
	t.Logf("║  FINAL: %.1f/100  Grade: %s                                ", finalPct, finalGrade)
	t.Logf("║  Weights: Dedup=%d  Journal=%d  Accuracy=%d  Efficiency=%d", weightDedup, weightJournal, weightAccuracy, weightPerf)
	t.Logf("╚══════════════════════════════════════════════════════════════╝")
}

func reportCategory(t *testing.T, label string, r evalResult) {
	t.Helper()
	g := grade(float64(r.Score))
	t.Logf("[%s] %s — Score: %d/100 Grade: %s — %s", g, label, r.Score, g, r.Details)
}
