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
	return assemblePlanCore(assembleParams{
		primary:         input.GetPrimary(),
		fallback:        input.GetFallback(),
		llmError:        input.GetLlmError(),
		threshold:       input.GetThreshold(),
		maxAlternatives: input.GetMaxAlternatives(),
		taskBlank:       input.GetTaskBlank(),
	}), nil
}

// assembleParams is the shared input of the plan-assembly core, which both
// AssemblePlan (batch/sync flow) and AssembleFromCells (mutation fan-out)
// run so the two paths produce identical plans from identical step results.
type assembleParams struct {
	// primary holds the preferred step results (LLM-decomposed steps for
	// AssemblePlan, the sorted fan-out cells for AssembleFromCells).
	primary []*gen.StepCandidates
	// fallback holds the degraded whole-description results. The fan-out path
	// has no fallback leg and passes nil.
	fallback []*gen.StepCandidates
	llmError string
	// planError is attribution raised UPSTREAM of the steps — a condition that
	// changed the shape of the plan itself (the fan-out's TRUNCATED). It leads
	// the joined error field: it is the most root-cause clause there is.
	planError string
	// leadingErrors are per-cell anomalies the fan-out path noticed while
	// projecting cells onto steps. They TRAIL the root-cause clauses, and are
	// dropped entirely when an upstream failure already explains an empty plan.
	leadingErrors   []string
	threshold       float64
	maxAlternatives int32
	taskBlank       bool
	// attributeLLMError surfaces a non-empty llmError even when a usable plan
	// basis exists. AssemblePlan leaves it false (its llm_error is only
	// meaningful on the fallback path, where the clause is emitted anyway);
	// the fan-out path sets it, because there the LLM error is the only
	// explanation a caller ever gets for a thin or empty plan.
	attributeLLMError bool
}

func assemblePlanCore(p assembleParams) *gen.PlanResult {
	threshold := p.threshold
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	maxAlt := int(p.maxAlternatives)
	if maxAlt <= 0 {
		maxAlt = 3
	}

	// A blank task is refused before anything else is weighed, and its
	// attribution is the bare NO_INPUT — every other clause a fan-out could
	// raise here (a cell reporting the plan carried no queries, a failed
	// decomposition of nothing) is a CONSEQUENCE of the blank task, not
	// independent information. Keeping this verdict bare is also what makes
	// it identical to the batch planner's.
	if p.taskBlank {
		return &gen.PlanResult{PlanBasis: "none", Error: "NO_INPUT", BridgeStatus: "unchecked"}
	}

	// Clause order is ROOT CAUSE FIRST: what reshaped the plan, then what
	// failed upstream of it, then the consequences either of those had on
	// individual cells. Reading the error field left to right should read as
	// an explanation, not as a symptom list (R19 MINOR-3).
	var errParts []string
	if pe := strings.TrimSpace(p.planError); pe != "" {
		errParts = append(errParts, pe)
	}
	llmClause := ""
	if p.attributeLLMError && strings.TrimSpace(p.llmError) != "" {
		llmClause = "decompose: " + p.llmError
		errParts = append(errParts, llmClause)
	}
	// When the decomposition failed AND no cell owned a step, every per-cell
	// clause is just "there were no queries" restated — the same reasoning
	// that makes the blank-task verdict a bare NO_INPUT.
	if llmClause == "" || len(p.primary) > 0 {
		errParts = append(errParts, p.leadingErrors...)
	}
	noInput := func() *gen.PlanResult {
		return &gen.PlanResult{
			PlanBasis:    "none",
			Error:        strings.Join(append(errParts, "NO_INPUT"), "; "),
			BridgeStatus: "unchecked",
		}
	}
	basisSteps := p.primary
	basis := "decomposed"
	if len(basisSteps) == 0 {
		basisSteps = p.fallback
		basis = "fallback"
	}
	if len(basisSteps) == 0 {
		return noInput()
	}

	if basis == "fallback" && !p.attributeLLMError {
		if p.llmError != "" {
			errParts = append(errParts, "decompose: "+p.llmError)
		} else {
			errParts = append(errParts, "decompose: no decomposition available; planned from the whole description as one step")
		}
	}

	result := &gen.PlanResult{PlanBasis: basis, StepCount: int32(len(basisSteps)), BridgeStatus: "unchecked"}
	matched := 0
	for _, sc := range basisSteps {
		ps := &gen.PlanStep{Description: sc.GetQuery()}
		if sc.GetError() != "" {
			ps.Error = sc.GetError()
			errParts = append(errParts, fmt.Sprintf("search: %s: %s", sc.GetQuery(), sc.GetError()))
			result.Steps = append(result.Steps, ps)
			continue
		}
		best := bestCandidate(sc.GetCandidates())
		if best != nil {
			ps.Score = best.Score
			if best.Score >= threshold {
				ps.Matched = true
				ps.Node = best.Node
				ps.Package = best.Package
				ps.Version = best.Version
				ps.NodeDescription = best.Description
				for _, c := range sc.GetCandidates() {
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
				Description:  sc.GetQuery(),
				ProposalHint: gapHint(sc.GetQuery(), best),
			})
		}
		result.Steps = append(result.Steps, ps)
	}

	result.MatchedCount = int32(matched)
	result.Coverage = float64(matched) / float64(len(basisSteps))
	result.Feasible = matched == len(basisSteps)
	result.Error = strings.Join(errParts, "; ")
	return result
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
