package nodes

import (
	"context"
	"fmt"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

const (
	// defaultThreshold and defaultLimit are the calibrated defaults shared by
	// the batch path (SearchSteps + AssemblePlan) and the fan-out path, so a
	// fan-out plan and a batch plan agree on what "matched" means.
	defaultThreshold = 0.45
	defaultLimit     = 5
)

// SearchStepAt is the per-step search cell of the mutation-grown planner fan-out: it reads its OWN canvas row via reflection (PlanFanOut appends one cell per decomposed step into the flow's authored cell column, row k for step k), searches the live Axiom catalog for exactly that step's query, and reports the scored candidates plus a matched verdict. Every cell in the fan-out receives the same broadcast plan; the canvas row is what makes each instance search a different step. The result is wrapped in a single-element list so the downstream join collects every cell with one repeated-field pick, and the plan's broadcast knobs (threshold, max_alternatives, llm_error, task_blank) plus any caller-visible plan-level attribution are echoed alongside it because the join's only inbound edges are cell edges. Every step is searched to the same fixed candidate depth as the batch SearchSteps path; max_alternatives caps the assembled runner-up list downstream and never narrows this search. Outside a flow (direct invoke) there is no canvas position, so the cell searches row 0 and notes NO_GRAPH in the cell's error. Reads the public catalog only — no authentication, no secrets.
func SearchStepAt(ctx context.Context, ax axiom.Context, input *gen.FanOutPlan) (*gen.FanOutCell, error) {
	cell := &gen.CellResult{}
	out := &gen.FanOutCell{
		Items:           []*gen.CellResult{cell},
		Threshold:       input.GetThreshold(),
		MaxAlternatives: input.GetMaxAlternatives(),
		LlmError:        input.GetLlmError(),
		TaskBlank:       input.GetTaskBlank(),
		PlanError:       input.GetPlanError(),
	}

	if len(input.GetQueries()) == 0 {
		cell.Error = "NO_QUERY: the plan carried no queries"
		return out, nil
	}

	fr := ax.Reflection().Flow()
	nodes := fr.Nodes()
	if len(nodes) == 0 {
		cell.Error = "NO_GRAPH: not running inside a flow; searching row 0"
	} else {
		self := fr.Position().CurrentInstance
		found := false
		for _, n := range nodes {
			if n.InstanceID == self {
				cell.Row = n.GridRow
				cell.Col = n.GridCol
				found = true
				break
			}
		}
		if !found {
			cell.Error = fmt.Sprintf("NO_POSITION: instance %d not in the reflection view; searching row 0", self)
		}
	}

	if int(cell.Row) >= len(input.GetQueries()) {
		cell.Error = fmt.Sprintf("ROW_OUT_OF_RANGE: this cell sits at row %d but the plan has only %d steps", cell.Row, len(input.GetQueries()))
		return out, nil
	}
	cell.Query = input.GetQueries()[cell.Row]

	threshold := input.GetThreshold()
	if threshold == 0 {
		threshold = defaultThreshold
	}

	// Search to the SAME fixed depth the batch path uses (the sync planner
	// wires SearchSteps with no limit mapping, so it always fetches
	// defaultLimit). max_alternatives caps the ASSEMBLED runner-up list and
	// nothing else — coupling it to the search limit here made the fan-out
	// report max_alternatives-1 runner-ups where the batch path reports
	// min(max_alternatives, defaultLimit-1), and made max_alternatives=1
	// report none at all (R19 MAJOR-1). The knob still rides the plan through
	// this cell, because the echo is the join's only route to it.
	cell.Step = searchOneQuery(ctx, cell.Query, defaultLimit)
	if len(cell.Step.GetCandidates()) > 0 {
		cell.Score = cell.Step.GetCandidates()[0].GetScore()
		cell.Matched = cell.Score >= threshold
	}
	return out, nil
}
