# UI Overhaul Plan — trading-bot web

Synthesis of two role analyses (Information Architect + Senior UI/UX Designer), 2026-08-21.
Stack stays: Go `html/template` + plain CSS + vanilla JS, no framework/build step. Dark-first,
token-driven (slate default + industrial amber), bilingual EN/zh-TW. Desktop-primary, mobile must work.

## The diagnosis (where both roles converged)

1. **Desktop layout doesn't exist.** `main{max-width:720px}` (style.css:139) caps EVERY page to a
   phone-width column on a desktop monitor (only /chart escapes). A data-dense trading app renders as
   a ribbon with huge empty gutters. **Single biggest visual problem.**
2. **Mobile nav is broken.** 11 flat nav links become a hidden-scrollbar horizontal strip on phones
   (style.css:4991-5006) — Ops & On-chain scroll off-screen with no affordance. Ops (restart daemon
   while out) is the highest mobile-need page and the least reachable.
3. **No scale system.** ~38 font-sizes, 13 radii, dozens of ad-hoc spacings → everything is slightly
   off from everything. Reads organically-grown, not designed.
4. **Token bypasses break the industrial theme.** Page-local `<style>` palettes in fundamentals.html /
   tw.html / calendar.html hardcode the status colors as literal rgba → those badges DON'T re-theme to
   industrial amber. Plus 33× hardcoded `#0f0b07`, 6× `#8b949e`, 8× `#b09a7f`.
5. **Tables cramped, no responsive story.** 0.82rem cells, nowrap, inconsistent overflow handling, no
   sticky headers, no priority columns, no mobile card fallback.
6. **11-item flat nav, no grouping/hierarchy.** Live-trading pages sit at equal weight with a spot
   screener, a macro reference, and a devops console. Stocks + TW are the SAME page twice.
7. **No true home.** `/` is "4-symbol scan", not a "what needs my attention now" surface.

## The target

### Information architecture — 11 flat items → 4 groups + a home
```
🏠 Home (/)            "Today" attention feed: blackout state · open-trade R · daemon health · pending setups
── TRADE ──   Chart · Scan (the 4-symbol grid) · Validate (fold toward Chart "+ at price")
── JOURNAL ── Portfolio (/journal) · Setups · Tips
── RESEARCH ─ Calendar · Fundamentals (US ⇄ TW merged, one page) · On-chain
── SYSTEM ──  Ops
```
- **Desktop**: grouped nav (Phase 0/1) → optional left sidebar with live status dot (Phase 2).
- **Mobile**: bottom tab bar (Home · Chart · Portfolio · Research · More) — fixes the broken strip.
  Daemon-down shows as a badge on More.
- **Command palette (⌘K)**: jump to page / symbol / TF / record / restart. Relieves nav pressure; ideal
  for a keyboard power-user. Every OLD page maps to a NEW group with zero features removed; only real
  consolidation is Stocks+TW → one Fundamentals page (US/TW toggle).

### Design system — build ON the existing tokens (don't replace)
- **Scale layer** on `:root`: space (4px base `--sp-1..8`), type (7 steps `--fs-xs..2xl`, mono+tabular
  for all numeric), radius (3: sm/md/lg + pill), elevation (border-step language + `--shadow-2` for
  floats), `--focus` ring.
- **Semantic overlay tokens** to kill bypasses: `--on-accent`, `--wash-{green,red,blue,yellow}`,
  `--row-hover`, `--th-bg` → delete the page-local `<style>` palettes; badges re-theme correctly.
- **Two color namespaces, documented**: UI status (`--green` etc, re-themes) vs data-viz
  (`--dv-up/--dv-down/--dv-vol`, constant across themes — candles must look the same). Formalizes the
  existing memory rule.
- **Canonical components**: `.dt` data table (sticky dim-caps header, roomier `--fs-base` cells, one
  `.dt-scroll` strategy, right-aligned `.num`, row hover); `.badge` (consolidates side-badge/fund-badge/
  score/oc-*/cal-cat/ot-flag); `.tile` (stat KPI); `.banner` (unifies macro/chart/tradeable banners);
  `.card` (keep the left-accent status variant); `.num` mixin (mono+tabular everywhere).

### Key screens
- **Chart (crown jewel)**: collapse 4 stacked control rows → one 44px toolbar; move 24h stats / scores /
  verdict / N字 struct into a **collapsible right panel** (TradingView model), bottom-sheet on mobile;
  verdict/score card is the panel hero (band-colored, already built). Record stays a modal.
- **Home/Dashboard**: command-center grid (open trades left+large, tradeable+portfolio-snapshot right),
  symbol scan becomes a responsive card grid (4-up desktop / 1-up phone), copy-buttons demote to hover,
  diagnostics behind a `▸ details` disclosure, merge the two stacked headers.
- **Data boards (fundamentals/tw)**: Bloomberg-screener feel — `.dt` table, sticky sortable headers,
  priority columns + column toggle, `.tile` summary row; **mobile → card-view** (tr→card via
  `td::before{content:attr(data-label)}`), NOT horizontal scroll.

### Mobile/responsive
- Kill `max-width:720px` → `max-width:1440px` + `clamp()` padding; internal grids use `auto-fit`+`minmax`.
- Breakpoints: sm 480 / md 768 / lg 1024 (desktop engages) / xl 1440.
- Tables collapse in tiers: full (≥lg) → priority cols + scroll (md-lg) → card-view (<md).
- Touch: 44px on `pointer:coarse`, 32px on `pointer:fine`. Copy tap-visible on coarse, hover on fine.

## Roadmap (merged, phased)

**Phase 0 — Foundations + quick wins (low effort, highest impact, no handler logic touched):**
1. Add scale + semantic-overlay + data-viz tokens to `:root` (pure addition).
2. **Kill `main{max-width:720px}`** → fluid width. Biggest single visual lift.
3. Delete the 3 page-local `<style>` palettes + hardcoded hex → tokens (fixes industrial theme).
4. Fix mobile nav (bottom tab / drawer) + group desktop nav into the 4 clusters.
5. Merge Stocks + TW → one Fundamentals page (US/TW toggle).

**Phase 1 — Component consolidation (medium effort, high impact):**
6. Canonical `.dt` table (reskins setups/fundamentals/tw/onchain at once).
7. Unify `.badge` / `.tile` / `.banner` primitives; migrate scattered variants.
8. Collapse the type scale (mechanical: ~38 sizes → 7).

**Phase 2 — Screen layouts (higher effort, high impact):**
9. "Today" home + Scan split; dashboard command-center grid.
10. Chart right-panel model (toolbar + collapsible right panel / bottom sheet). Crown jewel — most care.
11. Data-board mobile card-view + priority columns + sort.

**Phase 3 — Polish (incremental):**
12. Command palette ⌘K.
13. Density toggle, skeletons, generalized toasts, focus rings, i18n convergence (retire .i18n-en/zh
    span-pairs + data-lang-view → single data-i18n mechanism; fix `<html lang>`), grouped nav → sidebar.

**Ship-this-week trio (both roles agree):** #2 fluid width + #3 token bypasses + #6 `.dt` table — these
alone make it read as a professional terminal before any screen is redesigned.

## Constraints preserved
Dark-first + theme-aware throughout; data-viz colors stay un-themed (memory rule); bilingual; no
framework/build step; server-rendered. Every change additive to the token system.
