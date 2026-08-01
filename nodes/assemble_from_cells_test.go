package nodes_test

import (
	"context"
	"strings"
	"testing"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
	"google.golang.org/protobuf/proto"
)

// fanCell builds one cell's contribution the way SearchStepAt does.
func fanCell(row int32, query string, cands ...*gen.Candidate) *gen.CellResult {
	return &gen.CellResult{
		Row:   row,
		Col:   3,
		Query: query,
		Step:  &gen.StepCandidates{Query: query, Candidates: cands},
	}
}

// THE equivalence contract: the fan-out path and the batch path must produce
// the SAME plan from the same step results — that is what makes the v3 flow a
// drop-in replacement for the sync planner's search+assemble legs.
func TestAssembleFromCells_EquivalentToAssemblePlan(t *testing.T) {
	steps := []*gen.StepCandidates{
		{Query: "validate iban checksum", Candidates: []*gen.Candidate{
			cand("ValidateIban", "h/iban-tools", "0.1.2", 1.0),
			cand("CheckIbanCountry", "h/iban-tools", "0.1.2", 0.6),
			cand("FormatIban", "h/iban-tools", "0.1.2", 0.5),
		}},
		{Query: "look up bank by iban", Candidates: []*gen.Candidate{
			cand("LookupBank", "h/bank-tools", "2.0.0", 0.8),
		}},
		{Query: "summon a unicorn", Candidates: nil},
	}

	batch, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Primary: steps, Threshold: 0.5, MaxAlternatives: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cells []*gen.CellResult
	for i, s := range steps {
		cells = append(cells, fanCell(int32(i), s.Query, s.Candidates...))
	}
	fan, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells: cells, Threshold: 0.5, MaxAlternatives: 2,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !proto.Equal(batch, fan) {
		t.Fatalf("fan-out plan must equal the batch plan for the same steps:\n batch=%+v\n fan  =%+v", batch, fan)
	}
	if fan.PlanBasis != "decomposed" || fan.StepCount != 3 || fan.MatchedCount != 2 || fan.Feasible {
		t.Fatalf("sanity check on the shared verdict failed: %+v", fan)
	}
	if len(fan.Gaps) != 1 || fan.Gaps[0].Description != "summon a unicorn" {
		t.Fatalf("the uncovered step must be a gap: %+v", fan.Gaps)
	}
}

// The join collects members in edge order; the plan must depend on the cells'
// declared ROW instead, so a shuffled arrival can never reorder the plan.
func TestAssembleFromCells_SortsByRowNotArrivalOrder(t *testing.T) {
	inOrder := []*gen.CellResult{
		fanCell(0, "parse a vcard", cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)),
		fanCell(1, "validate an iban", cand("ValidateIban", "h/iban-tools", "0.1.2", 1.0)),
		fanCell(2, "render a qr code", cand("EncodeQr", "h/barcode-tools", "0.3.0", 1.0)),
	}
	shuffled := []*gen.CellResult{inOrder[2], inOrder[0], inOrder[1]}

	want, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{Cells: inOrder})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{Cells: shuffled})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proto.Equal(want, got) {
		t.Fatalf("arrival order must not affect the plan:\n want=%+v\n got =%+v", want, got)
	}
	for i, q := range []string{"parse a vcard", "validate an iban", "render a qr code"} {
		if got.Steps[i].Description != q {
			t.Fatalf("step %d out of row order: %q", i, got.Steps[i].Description)
		}
	}
}

// A cell that owned no step (empty plan, or a row past the end) must not mint
// a phantom step that would score as a gap — it is attributed instead.
func TestAssembleFromCells_CellsWithoutAStepAreAttributedNotCounted(t *testing.T) {
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells: []*gen.CellResult{
			fanCell(0, "parse a vcard", cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)),
			{Row: 1, Col: 3, Error: "ROW_OUT_OF_RANGE: this cell sits at row 1 but the plan has only 1 steps"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StepCount != 1 || got.MatchedCount != 1 || !got.Feasible {
		t.Fatalf("the stepless cell must not become a step: %+v", got)
	}
	if len(got.Gaps) != 0 {
		t.Fatalf("a stepless cell is not a gap: %+v", got.Gaps)
	}
	if !strings.Contains(got.Error, "cell row 1: ROW_OUT_OF_RANGE") {
		t.Fatalf("the anomaly must be attributed: %q", got.Error)
	}
}

// Every cell reporting NO_QUERY means there was nothing to plan at all — and
// the upstream LLM failure that caused it is the only explanation the caller
// gets, so it must be attributed.
func TestAssembleFromCells_NoUsableCellsIsNoInputWithLLMAttribution(t *testing.T) {
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells:    []*gen.CellResult{{Row: 0, Col: 3, Error: "NO_QUERY: the plan carried no queries"}},
		LlmError: "anthropic: 529 overloaded",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PlanBasis != "none" || got.Feasible || got.StepCount != 0 {
		t.Fatalf("no usable cells must be a 'none' basis: %+v", got)
	}
	if !strings.Contains(got.Error, "decompose: anthropic: 529 overloaded") || !strings.HasSuffix(got.Error, "NO_INPUT") {
		t.Fatalf("the LLM failure and NO_INPUT must both be attributed: %q", got.Error)
	}
	if got.BridgeStatus != "unchecked" {
		t.Fatalf("bridge_status must be unchecked for CheckBridges to fill in: %q", got.BridgeStatus)
	}
}

// The phantom-plan guard, end of the line: a task the caller knows was blank
// yields the same NO_INPUT verdict the sync planner gives, even if cells
// somehow carried results.
func TestAssembleFromCells_BlankTaskForcesNoInput(t *testing.T) {
	// The realistic shape: PlanFanOut refused, so the plan carried no queries
	// and the authored cell reports NO_QUERY. Those clauses are CONSEQUENCES
	// of the blank task and must not dilute the verdict — live-reproduced
	// against the sync planner, which emits a bare NO_INPUT.
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells: []*gen.CellResult{
			{Row: 0, Col: 3, Error: "NO_QUERY: the plan carried no queries"},
			fanCell(1, "invented step", cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)),
		},
		LlmError:  "anthropic: 529 overloaded",
		TaskBlank: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PlanBasis != "none" || got.Error != "NO_INPUT" || got.Feasible || len(got.Steps) != 0 {
		t.Fatalf("a blank task must yield a bare NO_INPUT with no steps: %+v", got)
	}

	// Same input, sync path: the two must agree.
	sync, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Primary:   []*gen.StepCandidates{{Query: "invented step", Candidates: []*gen.Candidate{cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)}}},
		TaskBlank: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proto.Equal(sync, got) {
		t.Fatalf("blank-task verdicts must match the sync planner:\n sync=%+v\n fan =%+v", sync, got)
	}
}

// A cell whose own search transport-failed is unmatched but NOT a gap —
// nothing was ruled out. Identical to the batch path's per-step error.
func TestAssembleFromCells_SearchTransportFailureIsNotAGap(t *testing.T) {
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells: []*gen.CellResult{
			fanCell(0, "parse a vcard", cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)),
			{Row: 1, Col: 3, Query: "validate an iban", Step: &gen.StepCandidates{Query: "validate an iban", Error: "dial tcp: connection refused"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StepCount != 2 || got.MatchedCount != 1 || got.Feasible {
		t.Fatalf("the failed search must be an unmatched step: %+v", got)
	}
	if len(got.Gaps) != 0 {
		t.Fatalf("an unreachable search rules nothing out — never a gap: %+v", got.Gaps)
	}
	if !strings.Contains(got.Error, "search: validate an iban: dial tcp: connection refused") {
		t.Fatalf("the transport failure must be attributed: %q", got.Error)
	}
}
