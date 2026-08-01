package nodes

import (
	"context"
	"fmt"
	"strings"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// AssemblePlan turns per-step catalog search results into a build plan: for each step it picks the best-scoring candidate node, marks the step matched when that score clears the threshold (default 0.45), lists runner-up alternatives, and emits an explicit gap ("what to build") for every step the catalog cannot cover. Prefers the primary (LLM-decomposed) results and falls back to the whole-description results when primary is empty, attributing the degradation in the error field ("decompose: ..."). A step whose search transport-failed is reported in error ("search: ...") and counted as unmatched but NOT as a gap — an unreachable search rules nothing out. plan_basis is always set ("decomposed" / "fallback" / "none") so a false feasible verdict is never ambiguous, and bridge_status is always "unchecked" — feed this result to CheckBridges to add per-pair type-bridge verdicts. Pure function of its input — no network, no secrets.
func AssemblePlan(ctx context.Context, ax axiom.Context, input *gen.AssemblePlanInput) (*gen.PlanResult, error) {
	threshold := input.Threshold
	if threshold <= 0 {
		threshold = 0.45
	}
	maxAlt := int(input.MaxAlternatives)
	if maxAlt <= 0 {
		maxAlt = 3
	}

	if input.TaskBlank {
		return &gen.PlanResult{PlanBasis: "none", Error: "NO_INPUT", BridgeStatus: "unchecked"}, nil
	}
	basisSteps := input.Primary
	basis := "decomposed"
	if len(basisSteps) == 0 {
		basisSteps = input.Fallback
		basis = "fallback"
	}
	if len(basisSteps) == 0 {
		return &gen.PlanResult{PlanBasis: "none", Error: "NO_INPUT", BridgeStatus: "unchecked"}, nil
	}

	var errParts []string
	if basis == "fallback" {
		if input.LlmError != "" {
			errParts = append(errParts, "decompose: "+input.LlmError)
		} else {
			errParts = append(errParts, "decompose: no decomposition available; planned from the whole description as one step")
		}
	}

	result := &gen.PlanResult{PlanBasis: basis, StepCount: int32(len(basisSteps)), BridgeStatus: "unchecked"}
	matched := 0
	for _, sc := range basisSteps {
		ps := &gen.PlanStep{Description: sc.Query}
		if sc.Error != "" {
			ps.Error = sc.Error
			errParts = append(errParts, fmt.Sprintf("search: %s: %s", sc.Query, sc.Error))
			result.Steps = append(result.Steps, ps)
			continue
		}
		best := bestCandidate(sc.Candidates)
		if best != nil {
			ps.Score = best.Score
			if best.Score >= threshold {
				ps.Matched = true
				ps.Node = best.Node
				ps.Package = best.Package
				ps.Version = best.Version
				ps.NodeDescription = best.Description
				for _, c := range sc.Candidates {
					if c == best || len(ps.Alternatives) >= maxAlt {
						continue
					}
					ps.Alternatives = append(ps.Alternatives, fmt.Sprintf("%s/%s@%s", c.Package, c.Node, c.Version))
				}
				matched++
			}
		}
		if !ps.Matched {
			result.Gaps = append(result.Gaps, &gen.Gap{
				Description:  sc.Query,
				ProposalHint: gapHint(sc.Query, best),
			})
		}
		result.Steps = append(result.Steps, ps)
	}

	result.MatchedCount = int32(matched)
	result.Coverage = float64(matched) / float64(len(basisSteps))
	result.Feasible = matched == len(basisSteps)
	result.Error = strings.Join(errParts, "; ")
	return result, nil
}

// bestCandidate returns the highest-scoring candidate (first wins ties, so an
// already-ranked list keeps its order), or nil for an empty list.
func bestCandidate(cs []*gen.Candidate) *gen.Candidate {
	var best *gen.Candidate
	for _, c := range cs {
		if best == nil || c.Score > best.Score {
			best = c
		}
	}
	return best
}

// gapHint writes the deterministic "what to build" suggestion for an
// unmatched step, naming the closest near-miss when one exists.
func gapHint(query string, best *gen.Candidate) string {
	if best != nil && best.Score > 0 {
		return fmt.Sprintf("No published node cleared the match threshold for %q (closest: %s/%s at %.2f) — consider proposing a package exposing this capability.",
			query, best.Package, best.Node, best.Score)
	}
	return fmt.Sprintf("No published node matched %q — consider proposing a package exposing this capability.", query)
}
