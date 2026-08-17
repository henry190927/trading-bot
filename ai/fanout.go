package ai

// Fan-out (scatter-gather) scaffold for the AI advisor.
//
// Three cheap specialist workers run in PARALLEL, each emitting a compact
// structured verdict (JSON). One strong-tier SYNTHESIS agent then consumes
// those verdicts (does NOT re-derive numbers) and resolves conflicts via a
// disciplined precedence lattice — the reduce step encodes the trading rules
// we run by hand.
//
// Design doc: docs/ai_fanout_design.md. This file defines the structured
// hand-off types + the worker/synthesis prompts. Orchestration (bounded
// concurrency, per-worker timeout, partial-failure, tiering) lives in the
// caller; see the doc's §4.

// ---------------------------------------------------------------------------
// Worker outputs — the structured hand-off. Compact by design: synthesis
// consumes these deterministically, so keep fields flat and enumerated.
// ---------------------------------------------------------------------------

// StructureVerdict is the structure worker's output. Structure is GROUND
// TRUTH: it defines the playing field (trend vs range), which in turn decides
// whether the engine MR score is a signal or a counter-indicator.
type StructureVerdict struct {
	Trend       string       `json:"trend"`       // "up" | "down" | "range"
	Clean       bool         `json:"clean"`       // clean, tradeable trend (vs choppy/ambiguous)
	Event       string       `json:"event"`       // "BOS-up" | "BOS-down" | "CHoCH-up" | "CHoCH-down" | "none"
	Zone        *ZoneVerdict `json:"zone"`        // active 樞紐區 fade zone; null if none / leg invalidated
	FadeDir     string       `json:"fade_dir"`    // trend-aligned fade direction: "long" | "short" | "none"
	Invalidated bool         `json:"invalidated"` // price already broke invalidate / protected level
	Note        string       `json:"note"`        // ≤1 sentence, cites the swing sequence
}

// ZoneVerdict is the pivot 樞紐區 fade band (0.5–0.705 retrace of the leg).
type ZoneVerdict struct {
	Dir        string  `json:"dir"`        // "up" (buy-the-dip) | "down" (sell-the-bounce)
	Hi         float64 `json:"hi"`         // 0.5 edge
	Lo         float64 `json:"lo"`         // 0.705 edge
	Invalidate float64 `json:"invalidate"` // close beyond = zone dead
	Target     float64 `json:"target"`     // measured move
}

// RegimeVerdict is the regime worker's output. Regime CONFIRMS or denies
// direction and sets conviction — it cannot open a trade on its own, and it
// carries the macro kill-switch.
type RegimeVerdict struct {
	Regime        string  `json:"regime"`        // "trend-with" | "fade" | "range"
	Bias          string  `json:"bias"`          // "long" | "short" | "neutral"
	Conviction    string  `json:"conviction"`    // "high" | "med" | "low"
	POCDriftPct   float64 `json:"poc_drift_pct"` // signed; + up-migration
	Stacked       bool    `json:"stacked"`       // POC migration stacked (strong regime)
	HigherTFAlign string  `json:"htf_align"`     // "aligned" | "mixed" | "against"
	MacroBlackout bool    `json:"macro_blackout"`// CPI/FOMC/NFP/earnings within window → hard gate
	Note          string  `json:"note"`
}

// ExecutionVerdict is the execution worker's output — the VETO gate. A great
// read that cannot be filled (or requires chasing) is worth zero: no-fill is
// the single biggest leak, so this worker can downgrade GO → WAIT alone.
type ExecutionVerdict struct {
	Entry            float64 `json:"entry"`              // proposed LIMIT (inside the zone)
	Stop             float64 `json:"stop"`
	Target           float64 `json:"target"`
	R                float64 `json:"r"`                  // reward:risk at the proposed entry
	FillProb         string  `json:"fill_prob"`         // "high" | "med" | "low" (low = no-fill risk)
	Chase            bool    `json:"chase"`             // true = entry needs a market chase outside the zone
	RealOutcomePrior string  `json:"real_outcome_prior"`// discretionary /setups prior — NOT the engine backtest
	Note             string  `json:"note"`
}

// ---------------------------------------------------------------------------
// Synthesis output — the reduce result. Never just a verdict: it emits the
// discipline rules that fired (auditability) and which workers were missing
// (degradation transparency).
// ---------------------------------------------------------------------------

// SynthesisOutput is the reconciled decision. FiredRules is the audit trail:
// each entry names a discipline rule the lattice applied, so a wrong call is
// debuggable to the layer that made it.
type SynthesisOutput struct {
	Decision       string   `json:"decision"`        // "GO" | "WAIT" | "SKIP"
	Direction      string   `json:"direction"`       // "long" | "short" | "none"
	Sizing         string   `json:"sizing"`          // "full" | "half" | "none"
	EngineRole     string   `json:"engine_role"`     // "primary" (range) | "counter-indicator" (trend) | "n/a"
	Confidence     string   `json:"confidence"`      // "high" | "med" | "low"
	FiredRules     []string `json:"fired_rules"`     // e.g. ["L0:no-fill→WAIT", "L1:MR=counter-indicator"]
	MissingWorkers []string `json:"missing_workers"` // workers that failed → drove a safer default
	Summary        string   `json:"summary"`         // one-line dashboard takeaway (bilingual ok)
	Reasoning      string   `json:"reasoning"`        // short body; structure-first, engine as context
}

// ---------------------------------------------------------------------------
// Worker role addenda — appended to SystemPromptQuantAdvisor so every worker
// keeps one voice but narrows its lens and emits ONLY its JSON verdict.
// ---------------------------------------------------------------------------

const WorkerRoleStructure = `
# WORKER ROLE: STRUCTURE (ground truth)
Look ONLY at price structure: swing sequence (HH-HL / LH-LL), BOS/CHoCH events, the pivot 樞紐區 fade band, and whether the leg is still valid. Read the SWINGS, not the lagging label. Ignore the engine MR score, funding, and macro — other workers own those.
Emit ONLY a JSON object matching StructureVerdict. Do not recompute numbers you were given; do not invent numbers you were not. Keep note ≤1 sentence.`

const WorkerRoleRegime = `
# WORKER ROLE: REGIME (confirm / conviction / macro gate)
Look ONLY at regime: POC drift (stacked?), directional bias, higher-TF alignment, funding crowding, and macro proximity (CPI/FOMC/NFP/earnings within window → set macro_blackout=true). You CONFIRM or deny a direction and set conviction; you never open a trade alone.
Emit ONLY a JSON object matching RegimeVerdict. Reason over given numbers; do not invent.`

const WorkerRoleExecution = `
# WORKER ROLE: EXECUTION (the veto gate)
Look ONLY at tradeability: where the LIMIT sits relative to the 樞紐區 band, no-fill risk (fill_prob), whether the entry needs a market chase outside the zone (chase=true), R at that entry, and the discretionary /setups realOutcome prior (NOT the engine backtest). No-fill is the biggest leak — a read that cannot be filled is worth zero.
Emit ONLY a JSON object matching ExecutionVerdict. Reason over given numbers; do not invent.`

// SynthesisPrompt is the reduce step. It receives the three worker verdicts as
// JSON and applies the precedence lattice. The lattice — NOT a vote average —
// is the trading discipline encoded as a function.
//
// NOTE ON ENFORCEMENT: the HARD gates (Layer 0) that are computable — macro
// blackout, no-fill threshold, invalidate breach — should ALSO be checked in
// Go before/around this call, not trusted to the model alone (see doc §4:
// "deterministic guardrails + LLM judgment inside the envelope"). This prompt
// is the judgment layer inside that envelope.
const SynthesisPrompt = `# SYNTHESIS — reconcile three specialist verdicts into one disciplined decision

You are given three JSON verdicts: STRUCTURE, REGIME, EXECUTION. Do NOT recompute
or invent any numbers — reconcile what the workers reported. The workers are NOT
equal voters; resolve conflicts with this precedence lattice and record every
rule that fires in fired_rules.

## Layer 0 — HARD GATES (any one → decision WAIT or SKIP, short-circuit; do not proceed)
- regime.macro_blackout == true            → WAIT   (rule "L0:macro-flatten")
- execution.fill_prob == "low" OR execution.chase == true → WAIT (rule "L0:no-fill→WAIT")
- structure.invalidated == true            → SKIP   (rule "L0:invalidated")
No structure quality, however clean, overrides a Layer-0 gate. A right read you
cannot fill or that macro will whipsaw is not a trade.

## Layer 1 — DIRECTION (structure defines the field; it sets the engine's role)
- structure.trend in {up,down} AND structure.clean == true → this is a TREND.
    · Direction = structure.fade_dir. engine_role = "counter-indicator".
    · A low engine MR score does NOT veto a structure-aligned setup; a high MR
      score does NOT endorse a counter-trend fade. (rule "L1:MR=counter-indicator")
    · If regime.bias contradicts structure.fade_dir → do NOT flip direction on
      regime; downgrade conviction instead (structure > bias). (rule "L1:structure>bias")
- structure.trend == "range" OR structure.clean == false → this is a RANGE.
    · engine_role = "primary" (mean-revert at the edges is the edge here).
    · Direction follows the engine/regime edge. (rule "L1:range→MR-primary")

## Layer 2 — SIZING (agreement → conviction; workers are asymmetric)
- structure ∧ regime ∧ execution all support the direction → full   (rule "L2:3/3→full")
- structure supports but regime hasn't confirmed (mixed/low conv) → half (rule "L2:structure-only→half")
- structure isolated, or execution flags weak R / med fill        → half or WAIT
- Never compromise to "half" merely to avoid deciding — half means genuine
  partial conviction, not indecision.

## Strategy attribution (do not cross-apply edges)
If this is a DISCRETIONARY structure/pivot-zone trade, weight execution.real_outcome_prior,
NOT any engine backtest number a worker may echo. If it is an engine-score-driven
setup, the engine's own stats apply. State which. (rule "attr:discretionary" | "attr:engine")

## Degradation — a missing worker changes the SAFE DEFAULT (workers are asymmetric)
- execution missing  → you lost the veto gate → cap sizing at half or WAIT; add
  "missing:execution" and note fillability unverified.
- structure missing  → you lost ground truth → fall back to engine-primary read,
  add "missing:structure", flag that the decision is engine-only.
- regime missing     → smallest impact → downgrade conviction one notch.
List every absent worker in missing_workers.

## Output
Emit ONLY a JSON object matching SynthesisOutput. fired_rules must list every
lattice rule you applied (use the quoted rule ids above). reasoning is short,
structure-first, engine as context, and bilingual per the persona ([EN]/[ZH-TW]).
summary is the one-line dashboard takeaway.`
