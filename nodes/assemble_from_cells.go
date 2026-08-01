package nodes

import (
	"context"
	"fmt"
	"sort"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// AssembleFromCells is the fan-in of the mutation-grown planner: it collects the per-step results of every SearchStepAt cell in the fan-out column and assembles them into the same build plan AssemblePlan produces from a batch search — best pick per step, matched verdict at the threshold (default 0.45), runner-up alternatives, and an explicit gap for every step the catalog cannot cover. Cells are sorted by their canvas row before assembly, so the order the join happened to collect them in never affects the plan. A cell that owns no step (the plan carried no queries, or its row fell outside the plan) contributes no phantom step; its attribution is carried in the error field instead. The error field reads root cause first: any plan-level condition that reshaped the plan upstream of the cells (a step list truncated at the fan-out cap), then an upstream decomposition failure, then the per-cell and per-step consequences. plan_basis is "decomposed" whenever at least one cell owned a step and "none" otherwise, and bridge_status is always "unchecked" — feed this result to CheckBridges to add per-pair type-bridge verdicts. Pure function of its input — no network, no secrets.
func AssembleFromCells(ctx context.Context, ax axiom.Context, input *gen.FanInInput) (*gen.PlanResult, error) {
	// Sort by canvas row: the join collects members in edge order, which the
	// platform keeps in row order for a replicated fan-out column, but the
	// plan must not DEPEND on that — row is the cell's declared identity.
	cells := append([]*gen.CellResult{}, input.GetCells()...)
	sort.SliceStable(cells, func(i, j int) bool { return cells[i].GetRow() < cells[j].GetRow() })

	var steps []*gen.StepCandidates
	var leading []string
	for _, c := range cells {
		if sc := cellToStep(c); sc != nil {
			steps = append(steps, sc)
			continue
		}
		// A cell with neither a query nor search results owned no step of the
		// plan (NO_QUERY, or a row past the end of the plan). Attribute it
		// rather than minting an empty step that would score as a gap.
		if e := c.GetError(); e != "" {
			leading = append(leading, fmt.Sprintf("cell row %d: %s", c.GetRow(), e))
		}
	}

	return assemblePlanCore(assembleParams{
		primary:           steps,
		llmError:          input.GetLlmError(),
		planError:         input.GetPlanError(),
		leadingErrors:     leading,
		threshold:         input.GetThreshold(),
		maxAlternatives:   input.GetMaxAlternatives(),
		taskBlank:         input.GetTaskBlank(),
		attributeLLMError: true,
	}), nil
}

// cellToStep projects one fan-out cell onto the step-results shape the plan
// core consumes, or nil when the cell owned no step at all. A cell-level
// failure that is not a search failure (e.g. ROW_OUT_OF_RANGE) is reported on
// the step's error, which the core counts as unmatched but NOT as a gap —
// nothing was ruled out.
func cellToStep(c *gen.CellResult) *gen.StepCandidates {
	src := c.GetStep()
	if src == nil && c.GetQuery() == "" {
		return nil
	}
	out := &gen.StepCandidates{
		Query:      src.GetQuery(),
		Candidates: src.GetCandidates(),
		Relaxed:    src.GetRelaxed(),
		Error:      src.GetError(),
		// The scoring basis and its degradation MUST survive the projection:
		// without them the fan-out plan would silently lose which stage ranked
		// each step, and assemble would fall back to the lexical threshold for
		// a semantically-scored step — a different verdict from the batch path
		// on identical results.
		ScoreBasis:   src.GetScoreBasis(),
		ScoringError: src.GetScoringError(),
	}
	if out.Query == "" {
		out.Query = c.GetQuery()
	}
	if out.Error == "" && c.GetError() != "" {
		out.Error = fmt.Sprintf("cell row %d: %s", c.GetRow(), c.GetError())
	}
	return out
}
