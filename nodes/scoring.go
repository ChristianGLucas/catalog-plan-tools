package nodes

import (
	"context"
	"strings"
	"sync"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// judgeSecret is the tenant secret that powers the rerank stage. Declared as a
// per-node required_secret (ADR-170) on both scoring nodes; the package never
// ships a key of its own, and a tenant without one still gets a plan — one
// stage weaker, and told so.
const judgeSecret = "ANTHROPIC_API_KEY"

// Scoring modes a caller may pin. Anything else means auto.
const (
	modeAuto     = ""
	modeJudge    = "judge"
	modeSemantic = "semantic"
	modeLexical  = "lexical"
)

// Matched/unmatched thresholds, calibrated per basis on the committed golden
// set (testdata/golden.json). They differ because the three scales are not
// comparable:
//
//   - lexical is a weighted word fraction: 0.45 was calibrated so that
//     generic-word overlap alone cannot clear it. UNCHANGED, so the fallback
//     path behaves exactly as the shipped planner did.
//   - semantic is a cosine similarity, which clusters far lower and far
//     tighter — on the golden set a correct node lands ~0.40-0.55 and a
//     wrong-domain one ~0.25-0.38, so the boundary sits at 0.38.
//   - judge is a calibrated probability the model was asked to produce
//     against a written rubric ("0.9-1.0 does exactly this step … 0.0-0.4
//     wrong job"). 0.55 keeps the "related but not this step" band unmatched,
//     which is what makes the no-real-match cases stay gaps.
const (
	thresholdLexical  = 0.45
	thresholdSemantic = 0.38
	thresholdJudge    = 0.55
)

// thresholdFor gives the calibrated default for a basis. A caller-supplied
// threshold always wins — it is documented as applying to whichever basis
// produced the ranking.
func thresholdFor(basis string) float64 {
	switch basis {
	case basisJudge:
		return thresholdJudge
	case basisSemantic:
		return thresholdSemantic
	default:
		return thresholdLexical
	}
}

// scoreStep is THE scoring stack, shared by SearchSteps (batch/sync) and
// SearchStepAt (fan-out cell) so both planners rank identically:
//
//	retrieve (semantic, falling back to lexical) -> judge rerank -> truncate
//
// Every degradation is attributed on the returned step's scoring_error and
// score_basis, and none of them fails the step: a plan ranked one stage weaker
// beats no plan.
func scoreStep(ctx context.Context, ax axiom.Context, query string, limit int, mode, model string) *gen.StepCandidates {
	sc := retrieve(ctx, query, limit, mode == modeLexical)

	if mode == modeLexical || mode == modeSemantic {
		// Pinned: report the stage the caller asked for, no LLM call.
		return truncate(sc, limit)
	}
	if sc.GetError() != "" {
		// Retrieval transport-failed outright; there is nothing to rerank.
		return truncate(sc, limit)
	}

	apiKey, ok := ax.Secrets().Get(judgeSecret)
	if !ok || strings.TrimSpace(apiKey) == "" {
		sc.ScoringError = joinClause(sc.GetScoringError(), judgeUnavailableClause(ax, sc.GetScoreBasis()))
		return truncate(sc, limit)
	}

	sc = judgeStep(ctx, sc, apiKey, model)
	return truncate(sc, limit)
}

// judgeUnavailableClause distinguishes the two ways a key can be missing, which
// need different fixes from the caller (ADR-156): a secret that was revoked
// mid-flight is not the same problem as one that was never configured.
func judgeUnavailableClause(ax axiom.Context, basis string) string {
	reason := "no " + judgeSecret + " configured for this tenant"
	if axiom.SecretStatusOf(ax.Secrets(), judgeSecret) == axiom.SecretStatusRevoked {
		reason = judgeSecret + " was revoked during this execution"
	}
	return "judge: " + reason + "; ranking " + basisAdverb(basis)
}

// scoreSteps runs the stack over a whole batch of steps CONCURRENTLY — one
// judge round-trip per step, all in flight together, so a 5-step plan costs one
// round-trip of wall time rather than five. (The fan-out planner gets the same
// property structurally: each cell judges its own step in its own node.)
// Bounded so a degenerate step list cannot open an unbounded number of
// connections to either the catalog or the provider.
func scoreSteps(ctx context.Context, ax axiom.Context, queries []string, limit int, mode, model string) []*gen.StepCandidates {
	const maxInFlight = 8

	out := make([]*gen.StepCandidates, len(queries))
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = scoreStep(ctx, ax, q, limit, mode, model)
		}(i, q)
	}
	wg.Wait()
	return out
}

// aggregateBasis summarises a plan's per-step bases: the single basis when the
// steps agree, "mixed" when they don't, "" when there is nothing to report.
// Honest reporting matters here — a plan where one step silently fell back to
// lexical is not a judged plan.
func aggregateBasis(steps []*gen.PlanStep) string {
	basis := ""
	for _, s := range steps {
		b := s.GetScoreBasis()
		if b == "" {
			continue
		}
		if basis == "" {
			basis = b
			continue
		}
		if basis != b {
			return "mixed"
		}
	}
	return basis
}
