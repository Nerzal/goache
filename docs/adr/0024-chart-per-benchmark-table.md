# 0024: A chart under every benchmark table; line charts for the size-parametrized ones

## Status

Accepted

## Context

README.md had six charts covering five of its twelve benchmark tables and
fenced blocks. The "Deletion" section had none, and of the seven
cross-library comparison tables only `Set` and `ParallelGet` were charted.

The two comparison charts were also the wrong form for their data. Each
comparison table is five size columns wide (1,000 → 1,000,000 entries, see
[ADR 0017](0017-size-parametrized-benchmarks.md)), and the whole point of
that structure is *how cost scales*. A bar chart shows one column;
[docs/benchcharts/main.go](../benchcharts/main.go) even carried a comment
apologising for it ("a bar chart can only show one size at a time, so
100,000 was kept as the representative point"). Four fifths of each table's
data never reached a reader who only looked at the picture, including the
scaling behaviour the takeaways below the tables argue about.

## Decision

**Every benchmark table and fenced block in README.md has a chart directly
beneath it** — at the time of writing thirteen charts for twelve tables (the
Deletion section gets two, see below). The rule, not the count, is what
holds: [ADR 0025](0025-cpu-constrained-benchmarks.md) later added two more
tables and two more charts under it.

**The seven cross-library tables became line charts**, one per table:
ns/op on a linear y-axis from zero, working-set size on a log-scaled x-axis
(the sizes are 1k/5k/50k/100k/1M). `renderLineChart` in
`docs/benchcharts/main.go` draws them; `renderBarChart` still handles the
single-value blocks, where a bar is the right form.

Design decisions, following the `dataviz` skill's procedure:

- **Colour is assigned per library and held constant across all seven
  charts** (`seriesColor` in main.go), so a reader who learns "blue is
  goache" on one chart keeps it on the next. Colour follows the entity,
  never its rank in a particular table.
- Slots are taken in order from that skill's validated categorical palette.
  The six-colour set was run through `scripts/validate_palette.js` against
  the `#ffffff` chart surface and passes every hard gate on the
  adjacent-pair list that line charts use: worst CVD ΔE 9.1, worst
  normal-vision ΔE 19.6. Three slots land under 3:1 contrast, which
  triggers the skill's relief rule — satisfied here because the source
  table sits immediately above every chart, so no value is reachable only
  through colour.
- goache is drawn thicker (3.5px vs 2px) and bolded in the legend rather
  than given a special colour: emphasis as a secondary encoding, not a
  second meaning for hue.
- Legends are ordered by cost at the largest size, cheapest first — the
  ranking a reader is actually looking for. With six converging series,
  direct end-labels would collide, so the legend carries identity and the
  table carries the values.

**The Deletion section gets two charts, not one.** Its fenced block mixes
two units: `Delete`/`DeleteMany`/`Delete+Set churn` are ns per *operation*,
while `SetManyRepeated`/`DeleteManyRepeated` are ns per *100-key call*.
Putting them on one axis rendered the per-operation bars as invisible
slivers next to 2,000+ ns/call bars — and would have implied a comparison
that isn't meaningful. Split into `deletion-ops.svg` (per operation) and
`bulk-reuse.svg` (per call, before vs after
[ADR 0022](0022-bulk-bucket-scratch-reuse.md)'s scratch reuse). `Clear` is
in neither: at ~10.9 ms it is five orders of magnitude away and would
flatten everything else.

The charts were rendered and inspected in a browser before being committed,
per the skill's final step — which is how the mixed-unit Deletion chart was
caught; it looked fine as source and wrong as a picture.

## Consequences

- Twelve tables, thirteen charts, no orphans. `CLAUDE.md`'s standing
  performance policy now says so explicitly, so a future table added
  without a chart is a documented omission rather than an oversight.
- `docs/benchcharts/main.go` grew a second renderer and a fixed
  library→colour map. Updating numbers means touching `bar{}` *or*
  `series{}` entries depending on the chart, then `make charts` as before.
- The comparison charts now carry all five size columns, so the scaling
  claims in README's takeaways ("every library's single-threaded Get
  degrades from 1,000 to 1,000,000 entries", "sharded designs hold their
  ground at every size", ristretto's inverted `Bounded` trend) are visible
  in the pictures that accompany them instead of only in prose.
- Charts remain light-surface only, matching the existing six. Dark-mode
  variants would need `<picture>` elements with `prefers-color-scheme`
  sources in README, which is a separate change to every embed and was not
  bundled here.
