// Command benchcharts regenerates the SVG bar charts embedded in README.md
// (docs/img/*.svg) from the benchmark numbers documented there. It is not
// part of the goache library — run it with `make charts` (or
// `go run ./docs/benchcharts`) whenever benchmark numbers in README.md
// change, and commit the regenerated SVGs alongside the numbers.
//
// Data below must be kept in sync with the fenced benchmark blocks in
// README.md by hand — there's no automated extraction, since the README
// numbers themselves are hand-curated from `go test -bench` output.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type bar struct {
	label string
	value float64
	// highlight marks the bar drawn in the "this is goache" color in
	// comparison charts; ignored for single-subject charts.
	highlight bool
}

const (
	chartWidth   = 640
	barHeight    = 28
	barGap       = 14
	leftMargin   = 190
	rightMargin  = 90
	topMargin    = 44
	bottomMargin = 16

	colorBar          = "#2563eb"
	colorBarHighlight = "#059669"
	colorText         = "#1f2933"
	colorAxis         = "#cbd5e1"
	colorBg           = "#ffffff"
)

// renderBarChart draws a horizontal bar chart, smallest value first (bars
// are "lower ns/op is better", so the fastest result reads at the top).
func renderBarChart(title, unit string, bars []bar) string {
	sorted := make([]bar, len(bars))
	copy(sorted, bars)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].value < sorted[j].value })

	maxVal := 0.0
	for _, b := range sorted {
		if b.value > maxVal {
			maxVal = b.value
		}
	}

	height := topMargin + bottomMargin + len(sorted)*(barHeight+barGap)
	plotWidth := chartWidth - leftMargin - rightMargin

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Segoe UI, Helvetica, Arial, sans-serif">`+"\n", chartWidth, height, chartWidth, height)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="%s"/>`+"\n", chartWidth, height, colorBg)
	fmt.Fprintf(&sb, `<text x="%d" y="24" font-size="16" font-weight="600" fill="%s">%s</text>`+"\n", leftMargin, colorText, escapeXML(title))

	for i, b := range sorted {
		y := topMargin + i*(barHeight+barGap)
		barWidth := 0.0
		if maxVal > 0 {
			barWidth = (b.value / maxVal) * float64(plotWidth)
		}
		color := colorBar
		if b.highlight {
			color = colorBarHighlight
		}

		fmt.Fprintf(&sb, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="1"/>`+"\n",
			leftMargin, y-4, leftMargin, y+barHeight+4, colorAxis)
		fmt.Fprintf(&sb, `<text x="%d" y="%d" font-size="13" fill="%s" text-anchor="end">%s</text>`+"\n",
			leftMargin-10, y+barHeight/2+5, colorText, escapeXML(b.label))
		fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="%.1f" height="%d" rx="3" fill="%s"/>`+"\n",
			leftMargin, y, barWidth, barHeight, color)
		fmt.Fprintf(&sb, `<text x="%.1f" y="%d" font-size="13" fill="%s">%s</text>`+"\n",
			float64(leftMargin)+barWidth+8, y+barHeight/2+5, colorText, escapeXML(formatValue(b.value)+" "+unit))
	}

	sb.WriteString(`</svg>` + "\n")
	return sb.String()
}

func formatValue(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func write(dir, name, svg string) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(svg), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write", path, ":", err)
		os.Exit(1)
	}
	fmt.Println("wrote", path)
}

func main() {
	outDir := "docs/img"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Core operations, single-threaded and parallel — from README.md
	// "Benchmarks" section (BenchmarkSet/SetMany/Get/GetMiss/
	// ParallelGetSet/ParallelGet).
	write(outDir, "core-ops.svg", renderBarChart("goache core operations (ns/op, lower is better)", "ns/op", []bar{
		{label: "Set", value: 31.75},
		{label: "SetMany", value: 98.16},
		{label: "Get", value: 23.84},
		{label: "GetMiss", value: 56.86},
		{label: "ParallelGetSet", value: 6.160},
		{label: "ParallelGet", value: 4.621},
		{label: "Delete", value: 40.33},
		{label: "DeleteMany", value: 71.58},
		{label: "Delete+Set churn", value: 88.08},
	}))

	// WithCapacity ingestion comparison — from README.md "Ingestion"
	// section (BenchmarkFreshLoad_NoHint / _WithCapacityHint).
	write(outDir, "capacity-hint.svg", renderBarChart("Fresh 10k-entry bulk load (ns/op, lower is better)", "ns/op", []bar{
		{label: "No hint", value: 1024314, highlight: false},
		{label: "WithCapacity(10000)", value: 864716, highlight: true},
	}))

	// Optional TTL overhead — from README.md "Optional TTL" section
	// (BenchmarkSet/Get vs BenchmarkSetWithTTL/GetWithTTL). Shows the cost
	// of TTL only on the path that actually uses it; the plain Set/Get
	// bars are unaffected (see docs/adr/0012-entry-ttl-field-size-cost.md).
	write(outDir, "ttl-overhead.svg", renderBarChart("TTL overhead: only the TTL path pays (ns/op, lower is better)", "ns/op", []bar{
		{label: "Get (no TTL)", value: 23.84, highlight: true},
		{label: "GetWithTTL", value: 28.08},
		{label: "Set (no TTL)", value: 31.75, highlight: true},
		{label: "SetWithTTL", value: 41.61},
	}))

	// Automatic eviction cost — from README.md "Automatic eviction" section
	// (BenchmarkSet vs BenchmarkSetWithMaxSize/BenchmarkEvictionChurn).
	// Shows the CLOCK eviction sweep's marginal cost on top of plain Set;
	// see docs/adr/0016-clock-eviction.md.
	write(outDir, "eviction-cost.svg", renderBarChart("WithMaxSize eviction cost on Set (ns/op, lower is better)", "ns/op", []bar{
		{label: "Set (unbounded)", value: 31.75, highlight: true},
		{label: "SetWithMaxSize (~50% evict)", value: 85.35},
		{label: "EvictionChurn (always evicts)", value: 114.6},
	}))

	// Cross-library comparison — from README.md "Comparison with other Go
	// cache libraries" section (bench/compare_test.go). Values are the
	// n=100,000 row of the full size-parametrized "Set"/"ParallelGet"
	// tables (see docs/adr/0017-size-parametrized-benchmarks.md) — a bar
	// chart can only show one size at a time, so 100,000 was kept as the
	// representative point since it matches this comparison's original
	// single-size scope. Reflects the clean re-run documented in the
	// README's "these numbers are from a single clean run" note.
	write(outDir, "compare-set.svg", renderBarChart("Set at n=100,000: goache vs other Go cache libraries (ns/op, lower is better)", "ns/op", []bar{
		{label: "goache", value: 32.63, highlight: true},
		{label: "go-cache", value: 39.11},
		{label: "freecache", value: 86.50},
		{label: "otter", value: 137.6},
		{label: "theine", value: 200.4},
		{label: "ristretto", value: 287.4},
	}))

	write(outDir, "compare-parallel-get.svg", renderBarChart("Parallel Get at n=100,000: goache vs other Go cache libraries (ns/op, lower is better)", "ns/op", []bar{
		{label: "otter", value: 5.980},
		{label: "theine", value: 6.324},
		{label: "goache", value: 4.578, highlight: true},
		{label: "ristretto", value: 8.674},
		{label: "freecache", value: 15.66},
		{label: "go-cache", value: 36.61},
	}))
}
