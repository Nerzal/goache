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
	"math"
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

// series is one library's row in a size-parametrized comparison table:
// values[i] is its ns/op at sizes[i].
type series struct {
	label  string
	values []float64
	// emphasis draws this series thicker — a secondary encoding for
	// "this is goache", so identity never rests on color alone.
	emphasis bool
}

// cpuCounts are the GOMAXPROCS values the CPU-limit sweep is measured at
// (`make bench-cpu`), matching README.md's column headers there.
var cpuCounts = []float64{1, 2, 4, 8, 24}

func formatCores(v float64) string { return fmt.Sprintf("%.0f", v) }

// sizes are the working-set sizes every comparison table is measured at,
// matching README.md's column headers.
var sizes = []float64{1000, 5000, 50000, 100000, 1000000}

// seriesColor assigns each library a fixed hue for its identity, held
// constant across every chart — color follows the entity, never its rank in
// a particular table. Slots are taken in order from the validated
// categorical palette (see the dataviz reference palette); the set of six
// passes the CVD and normal-vision separation gates on the adjacent-pair
// list that line charts use.
var seriesColor = map[string]string{
	"goache":    categorical[0], // blue
	"go-cache":  categorical[1], // orange
	"freecache": categorical[2], // aqua
	"otter":     categorical[3], // yellow
	"theine":    categorical[4], // magenta
	"ristretto": categorical[5], // green
}

// categorical is the validated palette in slot order. Charts whose series
// are not libraries (the per-benchmark CPU chart) take slots from here by
// position instead; either way hues are assigned in fixed order and never
// cycled.
var categorical = []string{
	"#2a78d6", // 1 blue
	"#eb6834", // 2 orange
	"#1baf7a", // 3 aqua
	"#eda100", // 4 yellow
	"#e87ba4", // 5 magenta
	"#008300", // 6 green
}

// colorFor returns the fixed hue for a library, or the slot at index i for
// series that are not libraries. Panics rather than drawing an invisible
// line if a chart ever exceeds the validated palette.
func colorFor(label string, i int) string {
	if c, ok := seriesColor[label]; ok {
		return c
	}
	if i >= len(categorical) {
		panic(fmt.Sprintf("chart needs %d series, palette validated for %d", i+1, len(categorical)))
	}
	return categorical[i]
}

const (
	chartWidth   = 640
	barHeight    = 28
	barGap       = 14
	leftMargin   = 190
	rightMargin  = 120 // fits a 7-digit value plus its unit without clipping
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

const (
	linePlotWidth   = 550 // plot area, held constant across charts
	lineChartHeight = 380
	lineLeft        = 62 // room for y-axis tick labels
	lineTop         = 52
	lineBottom      = 46
	// legendCharWidth approximates the advance width of the 12px label font.
	// The legend column is sized from the longest label so a long series
	// name widens the image instead of being clipped by it.
	legendCharWidth = 6.7
	legendGutter    = 24 // gap between plot and legend
	legendKeyWidth  = 26 // swatch line plus its spacing
	legendPadRight  = 16

	colorTextMuted = "#6b7280"
	colorGrid      = "#e5e7eb"
)

// niceCeil rounds v up to a clean axis maximum (1/2/2.5/5 x 10^n), so the
// y-axis carries round numbers rather than whatever the data happened to
// peak at.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Floor(math.Log10(v))
	pow := math.Pow(10, exp)
	switch f := v / pow; {
	case f <= 1:
		return pow
	case f <= 2:
		return 2 * pow
	case f <= 2.5:
		return 2.5 * pow
	case f <= 5:
		return 5 * pow
	default:
		return 10 * pow
	}
}

func formatTick(v float64) string {
	switch {
	case v >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func formatSize(n float64) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.0fM", n/1000000)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", n/1000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

// renderLineChart draws ns/op against some scaling axis for several
// libraries at once. The x-axis is log-scaled because both axes it serves
// span orders of magnitude (working-set sizes 1k..1M, core counts 1..24);
// the y-axis stays linear and starts at zero so the vertical distance
// between two lines is proportional to the actual difference in cost.
//
// A bar chart can only show one column, which is why the earlier
// single-size comparison charts were replaced: these tables are about how
// cost scales, and that is the axis a bar chart throws away.
func renderLineChart(title, unit, xTitle string, xs []float64, fmtX func(float64) string, all []series) string {
	maxVal := 0.0
	for _, s := range all {
		for _, v := range s.values {
			if v > maxVal {
				maxVal = v
			}
		}
	}
	yMax := niceCeil(maxVal)

	longest := 0
	for _, s := range all {
		if len(s.label) > longest {
			longest = len(s.label)
		}
	}
	lineRight := legendGutter + legendKeyWidth + int(float64(longest)*legendCharWidth) + legendPadRight
	lineChartWidth := lineLeft + linePlotWidth + lineRight

	plotW := float64(linePlotWidth)
	plotH := float64(lineChartHeight - lineTop - lineBottom)
	logMin, logMax := math.Log10(xs[0]), math.Log10(xs[len(xs)-1])

	x := func(v float64) float64 {
		return float64(lineLeft) + (math.Log10(v)-logMin)/(logMax-logMin)*plotW
	}
	y := func(v float64) float64 {
		return float64(lineTop) + (1-v/yMax)*plotH
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="Segoe UI, Helvetica, Arial, sans-serif">`+"\n",
		lineChartWidth, lineChartHeight, lineChartWidth, lineChartHeight)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="%s"/>`+"\n", lineChartWidth, lineChartHeight, colorBg)
	fmt.Fprintf(&sb, `<text x="%d" y="26" font-size="16" font-weight="600" fill="%s">%s</text>`+"\n",
		lineLeft-40, colorText, escapeXML(title))
	fmt.Fprintf(&sb, `<text x="%d" y="44" font-size="12" fill="%s">%s — lower is better</text>`+"\n",
		lineLeft-40, colorTextMuted, escapeXML(unit))

	// Horizontal gridlines with y-axis ticks: hairline, solid, recessive.
	const yTicks = 4
	for i := 0; i <= yTicks; i++ {
		v := yMax * float64(i) / yTicks
		yy := y(v)
		fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`+"\n",
			float64(lineLeft), yy, float64(lineLeft)+plotW, yy, colorGrid)
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="end">%s</text>`+"\n",
			float64(lineLeft)-8, yy+4, colorTextMuted, escapeXML(formatTick(v)))
	}

	// X-axis ticks, one per measured point.
	for _, v := range xs {
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">%s</text>`+"\n",
			x(v), float64(lineTop)+plotH+20, colorTextMuted, escapeXML(fmtX(v)))
	}
	fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">%s</text>`+"\n",
		float64(lineLeft)+plotW/2, float64(lineTop)+plotH+38, colorTextMuted, escapeXML(xTitle))

	// Draw the emphasized series last so it sits on top where lines cross.
	order := make([]int, 0, len(all))
	for i, s := range all {
		if !s.emphasis {
			order = append(order, i)
		}
	}
	for i, s := range all {
		if s.emphasis {
			order = append(order, i)
		}
	}

	for _, idx := range order {
		s := all[idx]
		color := colorFor(s.label, idx)
		width := 2.0
		if s.emphasis {
			width = 3.5
		}

		var path strings.Builder
		for i, v := range s.values {
			if i == 0 {
				fmt.Fprintf(&path, "M%.1f %.1f", x(xs[i]), y(v))
				continue
			}
			fmt.Fprintf(&path, " L%.1f %.1f", x(xs[i]), y(v))
		}
		fmt.Fprintf(&sb, `<path d="%s" fill="none" stroke="%s" stroke-width="%.1f" stroke-linejoin="round" stroke-linecap="round"/>`+"\n",
			path.String(), color, width)

		// Markers carry a surface-colored ring so they stay legible where
		// series cross each other.
		for i, v := range s.values {
			fmt.Fprintf(&sb, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="%s" stroke-width="2"/>`+"\n",
				x(xs[i]), y(v), color, colorBg)
		}
	}

	// Legend — always present for two or more series, so identity never
	// rests on color alone. Ordered by cost at the largest size, cheapest
	// first, which is the ranking a reader is looking for.
	type legendEntry struct {
		series
		color string
	}
	legend := make([]legendEntry, len(all))
	for i, s := range all {
		legend[i] = legendEntry{series: s, color: colorFor(s.label, i)}
	}
	sort.SliceStable(legend, func(i, j int) bool {
		return legend[i].values[len(legend[i].values)-1] < legend[j].values[len(legend[j].values)-1]
	})
	legendX := float64(lineLeft) + plotW + 24
	for i, s := range legend {
		ly := float64(lineTop) + 6 + float64(i)*22
		fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="%.1f" stroke-linecap="round"/>`+"\n",
			legendX, ly, legendX+18, ly, s.color, map[bool]float64{true: 3.5, false: 2}[s.emphasis])
		fmt.Fprintf(&sb, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="%s" stroke-width="2"/>`+"\n",
			legendX+9, ly, s.color, colorBg)
		weight := "400"
		if s.emphasis {
			weight = "600"
		}
		fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="12" font-weight="%s" fill="%s">%s</text>`+"\n",
			legendX+26, ly+4, weight, colorText, escapeXML(s.label))
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

// renderSizeChart is renderLineChart bound to the working-set-size axis
// every cross-library comparison table uses.
func renderSizeChart(title, unit string, all []series) string {
	return renderLineChart(title, unit, "entries", sizes, formatSize, all)
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
		{label: "Set", value: 30.84},
		{label: "SetMany", value: 103.4},
		{label: "Get", value: 22.94},
		{label: "GetMiss", value: 56.81},
		{label: "ParallelGetSet", value: 6.236},
		{label: "ParallelGet", value: 4.601},
		{label: "Delete", value: 39.73},
		{label: "DeleteMany", value: 70.53},
		{label: "Delete+Set churn", value: 85.54},
	}))

	// WithCapacity ingestion comparison — from README.md "Ingestion"
	// section (BenchmarkFreshLoad_NoHint / _WithCapacityHint).
	write(outDir, "capacity-hint.svg", renderBarChart("Fresh 10k-entry bulk load (ns/op, lower is better)", "ns/op", []bar{
		{label: "No hint", value: 1047438, highlight: false},
		{label: "WithCapacity(10000)", value: 816126, highlight: true},
	}))

	// Optional TTL overhead — from README.md "Optional TTL" section
	// (BenchmarkSet/Get vs BenchmarkSetWithTTL/GetWithTTL). Shows the cost
	// of TTL only on the path that actually uses it; the plain Set/Get
	// bars are unaffected (see docs/adr/0012-entry-ttl-field-size-cost.md).
	write(outDir, "ttl-overhead.svg", renderBarChart("TTL overhead: only the TTL path pays (ns/op, lower is better)", "ns/op", []bar{
		{label: "Get (no TTL)", value: 22.94, highlight: true},
		{label: "GetWithTTL", value: 28.54},
		{label: "Set (no TTL)", value: 30.84, highlight: true},
		{label: "SetWithTTL", value: 41.96},
	}))

	// Automatic eviction cost — from README.md "Automatic eviction" section
	// (BenchmarkSet vs BenchmarkSetWithMaxSize/BenchmarkEvictionChurn).
	// Shows the CLOCK eviction sweep's marginal cost on top of plain Set;
	// see docs/adr/0016-clock-eviction.md.
	write(outDir, "eviction-cost.svg", renderBarChart("WithMaxSize eviction cost on Set (ns/op, lower is better)", "ns/op", []bar{
		{label: "Set (unbounded)", value: 30.84, highlight: true},
		{label: "SetWithMaxSize (~50% evict)", value: 72.63},
		{label: "EvictionChurn (always evicts)", value: 115.0},
	}))

	// Deletion — from README.md "Deletion" section (BenchmarkDelete/
	// DeleteMany/DeleteSetChurn, see docs/adr/0019). Per-operation costs
	// only. Clear is left out: at ~10.9ms it is five orders of magnitude
	// away and would flatten every other bar to nothing, and the repeated
	// bulk calls below are per *call* rather than per operation, so they
	// get their own chart rather than sharing an axis with these.
	write(outDir, "deletion-ops.svg", renderBarChart("Deletion cost per operation (ns/op, lower is better)", "ns/op", []bar{
		{label: "Delete", value: 39.73},
		{label: "DeleteMany", value: 70.53},
		{label: "Delete+Set churn", value: 85.54},
	}))

	// Repeated bulk calls, before and after the scratch-space reuse in
	// docs/adr/0022. These are per 100-key call, not per key, which is why
	// they are not on the same axis as the per-operation chart above.
	write(outDir, "bulk-reuse.svg", renderBarChart("Repeated bulk calls, per 100-key call (ns/call, lower is better)", "ns/call", []bar{
		{label: "SetMany before", value: 5375},
		{label: "SetMany after", value: 2418, highlight: true},
		{label: "DeleteMany before", value: 4885},
		{label: "DeleteMany after", value: 2014, highlight: true},
	}))

	// Cross-library comparison — from README.md "Comparison with other Go
	// cache libraries" section (bench/compare_test.go), one chart per
	// table. These are line charts rather than bars because the tables are
	// size-parametrized (see docs/adr/0017-size-parametrized-benchmarks.md)
	// and the story in them is how cost scales from 1,000 to 1,000,000
	// entries — the axis a single-size bar chart throws away.
	write(outDir, "compare-set.svg", renderSizeChart("Set: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{27.91, 23.78, 28.72, 32.63, 127.2}, emphasis: true},
		{label: "go-cache", values: []float64{28.27, 30.07, 35.28, 39.11, 130.6}},
		{label: "freecache", values: []float64{35.83, 36.63, 59.13, 86.50, 187.0}},
		{label: "otter", values: []float64{139.7, 130.7, 133.2, 137.6, 225.8}},
		{label: "theine", values: []float64{157.1, 158.0, 176.2, 200.4, 413.0}},
		{label: "ristretto", values: []float64{223.7, 236.4, 240.5, 287.4, 362.3}},
	}))

	write(outDir, "compare-get.svg", renderSizeChart("Get: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{22.77, 16.67, 21.01, 23.84, 87.98}, emphasis: true},
		{label: "go-cache", values: []float64{12.94, 12.56, 15.79, 17.40, 70.39}},
		{label: "otter", values: []float64{34.71, 35.01, 37.89, 40.79, 149.2}},
		{label: "ristretto", values: []float64{42.59, 41.93, 61.51, 79.35, 178.5}},
		{label: "freecache", values: []float64{47.67, 48.57, 72.66, 108.4, 208.3}},
		{label: "theine", values: []float64{87.17, 86.33, 111.6, 132.1, 286.4}},
	}))

	write(outDir, "compare-parallel-get.svg", renderSizeChart("Parallel Get: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{5.673, 4.573, 4.235, 4.578, 9.250}, emphasis: true},
		{label: "otter", values: []float64{3.262, 6.722, 7.479, 5.980, 9.934}},
		{label: "theine", values: []float64{5.258, 5.220, 5.978, 6.324, 10.52}},
		{label: "ristretto", values: []float64{9.079, 8.217, 7.534, 8.674, 10.88}},
		{label: "freecache", values: []float64{14.59, 14.95, 15.39, 15.66, 17.59}},
		{label: "go-cache", values: []float64{36.74, 37.01, 37.05, 36.61, 38.42}},
	}))

	write(outDir, "compare-set-ttl.svg", renderSizeChart("SetWithTTL: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{35.33, 30.91, 37.67, 42.10, 171.7}, emphasis: true},
		{label: "go-cache", values: []float64{34.41, 36.26, 46.66, 51.80, 201.2}},
		{label: "freecache", values: []float64{36.32, 38.01, 63.18, 88.99, 194.8}},
		{label: "otter", values: []float64{159.2, 159.2, 168.8, 174.2, 260.6}},
		{label: "theine", values: []float64{181.6, 176.5, 207.0, 261.3, 420.2}},
		{label: "ristretto", values: []float64{248.2, 253.1, 299.5, 372.8, 441.2}},
	}))

	write(outDir, "compare-get-ttl.svg", renderSizeChart("GetWithTTL: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{28.26, 21.08, 26.07, 28.35, 93.24}, emphasis: true},
		{label: "go-cache", values: []float64{14.10, 14.98, 20.59, 22.70, 94.21}},
		{label: "otter", values: []float64{47.96, 50.17, 54.57, 59.86, 163.2}},
		{label: "ristretto", values: []float64{47.16, 46.00, 58.42, 94.23, 187.8}},
		{label: "freecache", values: []float64{47.64, 48.50, 74.35, 106.1, 211.9}},
		{label: "theine", values: []float64{86.90, 83.13, 103.1, 136.0, 287.9}},
	}))

	write(outDir, "compare-delete.svg", renderSizeChart("Delete churn: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{65.60, 64.54, 82.52, 87.69, 198.0}, emphasis: true},
		{label: "go-cache", values: []float64{53.64, 58.12, 68.42, 72.00, 191.4}},
		{label: "freecache", values: []float64{83.20, 102.9, 159.0, 200.4, 274.5}},
		{label: "otter", values: []float64{225.0, 226.0, 243.7, 257.1, 361.5}},
		{label: "theine", values: []float64{408.9, 418.1, 431.3, 492.9, 583.1}},
		{label: "ristretto", values: []float64{485.0, 471.8, 514.6, 537.1, 649.0}},
	}))

	// goache's own behaviour across core counts — from README.md's
	// "Performance under a CPU limit" section (`make bench-cpu`). Get is
	// included as the flat reference line: it is single-goroutine, so
	// GOMAXPROCS does not move it, which is exactly what makes the
	// one-core convergence of the Parallel* lines onto it readable.
	write(outDir, "cpu-goache.svg", renderLineChart(
		"goache by available cores, 100,000 entries", "ns/op",
		"GOMAXPROCS (cores available to the process)",
		cpuCounts, formatCores,
		[]series{
			{label: "ParallelGet", values: []float64{23.36, 14.02, 7.506, 7.758, 4.614}, emphasis: true},
			{label: "ParallelGetConstrained", values: []float64{23.29, 14.58, 7.609, 7.762, 4.638}},
			{label: "ParallelGetWithMaxSize", values: []float64{26.74, 16.56, 8.687, 9.067, 4.794}},
			{label: "ParallelGetSet", values: []float64{27.47, 17.32, 11.58, 9.312, 6.044}},
			{label: "ParallelGetSetConstrained", values: []float64{25.98, 15.35, 9.609, 10.42, 6.639}},
			{label: "Get (single-goroutine)", values: []float64{25.02, 24.93, 25.27, 24.16, 24.24}},
		}))

	// CPU-constrained comparison — from README.md's "Performance under a CPU
	// limit" section (`make bench-compare-cpu`). x is GOMAXPROCS, which is
	// what a Kubernetes CPU limit actually controls under Go 1.25+: a pod
	// with `limits.cpu: 100m` runs the leftmost column. See
	// docs/adr/0025-cpu-constrained-benchmarks.md.
	write(outDir, "compare-cpu.svg", renderLineChart(
		"Concurrent Get at 100,000 entries, by available cores", "ns/op",
		"GOMAXPROCS (cores available to the process)",
		cpuCounts, formatCores,
		[]series{
			{label: "goache", values: []float64{24.51, 14.08, 7.399, 7.587, 4.687}, emphasis: true},
			{label: "go-cache", values: []float64{16.88, 25.50, 17.56, 27.29, 37.41}},
			{label: "otter", values: []float64{34.38, 24.86, 9.695, 5.899, 3.924}},
			{label: "ristretto", values: []float64{37.31, 32.37, 16.10, 15.99, 8.211}},
			{label: "freecache", values: []float64{103.7, 52.92, 27.16, 26.02, 16.18}},
			{label: "theine", values: []float64{133.5, 81.79, 75.17, 12.57, 6.443}},
		}))

	write(outDir, "compare-bounded.svg", renderSizeChart("Bounded eviction: goache vs other Go cache libraries", "ns/op", []series{
		{label: "goache", values: []float64{47.52, 61.31, 69.12, 110.4, 269.8}, emphasis: true},
		{label: "ristretto", values: []float64{125.5, 108.6, 104.0, 89.94, 76.34}},
		{label: "otter", values: []float64{140.9, 131.3, 156.1, 159.9, 237.3}},
		{label: "theine", values: []float64{199.4, 190.3, 184.8, 233.1, 347.5}},
	}))
}
