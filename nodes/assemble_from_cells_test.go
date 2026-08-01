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

// R19 MAJOR-1 regression, at the level the defect actually lived: run the SAME
// queries through the WHOLE batch path (SearchSteps -> AssemblePlan) and the
// WHOLE fan-out path (one SearchStepAt per canvas row -> AssembleFromCells)
// against one fake catalog, at NON-DEFAULT max_alternatives. Coupling the
// cell's search limit to the knob made these diverge: v3 listed
// max_alternatives-1 runner-ups where the batch path lists
// min(max_alternatives, defaultLimit-1), and max_alternatives=1 listed none.
func TestFanOutPathMatchesBatchPath_AtNonDefaultMaxAlternatives(t *testing.T) {
	queries := []string{"validate iban checksum", "parse a vcard contact"}
	fakeSearch(t, map[string][]map[string]string{
		"validate iban checksum": {
			apiRow("ValidateIban", "h/iban-tools", "0.2.0", "Validate an IBAN's checksum and structure"),
			apiRow("CheckIbanCountry", "h/iban-tools", "0.2.0", "Check an IBAN's country against an expected checksum-bearing code"),
			apiRow("ValidateBic", "h/iban-tools", "0.2.0", "Validate a BIC structure"),
			apiRow("IsQrIban", "h/iban-tools", "0.2.0", "Detect a QR-IBAN"),
			apiRow("ComposeIban", "h/iban-tools", "0.2.0", "Compose an IBAN"),
		},
		"parse a vcard contact": {
			apiRow("ParseVCard", "h/vcard-tools", "0.1.2", "Parse a vCard contact document"),
			apiRow("ParseVCardList", "h/vcard-tools", "0.1.2", "Parse a list of vCard contacts"),
			apiRow("FormatVCard", "h/vcard-tools", "0.1.2", "Format a vCard contact"),
			apiRow("ExtractPhoto", "h/vcard-tools", "0.1.2", "Extract a vCard photo"),
			apiRow("ListFields", "h/vcard-tools", "0.1.2", "List a vCard's fields"),
		},
	})

	// 1 is the sharpest case: the pre-fix code returned ZERO alternatives.
	for _, maxAlt := range []int32{1, 2, 10} {
		batchSearch, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{Queries: queries})
		if err != nil {
			t.Fatalf("maxAlt=%d: %v", maxAlt, err)
		}
		batch, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
			Primary: batchSearch.GetSteps(), MaxAlternatives: maxAlt,
		})
		if err != nil {
			t.Fatalf("maxAlt=%d: %v", maxAlt, err)
		}

		var cells []*gen.CellResult
		var echo *gen.FanOutCell
		for row := range queries {
			out, err := nodes.SearchStepAt(context.Background(), cellAt(t, 3, int32(row)),
				&gen.FanOutPlan{Queries: queries, FanoutCol: 3, MaxAlternatives: maxAlt})
			if err != nil {
				t.Fatalf("maxAlt=%d row=%d: %v", maxAlt, row, err)
			}
			echo = out
			cells = append(cells, out.GetItems()...)
		}
		// The knob must not narrow the SEARCH — every cell still reports the
		// full candidate depth, and only the assembled list is capped.
		if n := len(cells[0].GetStep().GetCandidates()); n != 5 {
			t.Fatalf("maxAlt=%d: the cell's search depth must be independent of the knob, got %d candidates", maxAlt, n)
		}
		fan, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
			Cells: cells, MaxAlternatives: echo.GetMaxAlternatives(),
		})
		if err != nil {
			t.Fatalf("maxAlt=%d: %v", maxAlt, err)
		}

		if !proto.Equal(batch, fan) {
			t.Fatalf("maxAlt=%d: fan-out plan must equal the batch plan:\n batch=%+v\n fan  =%+v", maxAlt, batch, fan)
		}
		want := int(maxAlt)
		if want > 4 {
			want = 4 // defaultLimit - 1: the best pick is never its own alternative
		}
		if got := len(fan.GetSteps()[0].GetAlternatives()); got != want {
			t.Fatalf("maxAlt=%d: want %d alternatives per step, got %d", maxAlt, want, got)
		}
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
	// R19 MINOR-3: root cause first, and the per-cell clause is dropped
	// entirely — with no queries to own, "this cell owned no step" restates
	// the decomposition failure rather than adding to it.
	if got.Error != "decompose: anthropic: 529 overloaded; NO_INPUT" {
		t.Fatalf("the LLM failure must LEAD, with the cell consequence dropped: %q", got.Error)
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

// R19 MINOR-2, consumer end: a plan truncated upstream of the cells must say
// so, and must say it FIRST — the caller is holding a plan shorter than the
// task decomposed to, which no per-step clause can convey.
func TestAssembleFromCells_PlanErrorLeadsAndReachesTheCaller(t *testing.T) {
	const trunc = "TRUNCATED: 20 steps exceed the fan-out cap of 16; planning the first 16"
	got, err := nodes.AssembleFromCells(context.Background(), newTestContext(t), &gen.FanInInput{
		Cells: []*gen.CellResult{
			fanCell(0, "parse a vcard", cand("ParseVCard", "h/vcard-tools", "0.1.0", 1.0)),
			{Row: 1, Col: 3, Query: "validate an iban", Step: &gen.StepCandidates{Query: "validate an iban", Error: "dial tcp: connection refused"}},
		},
		PlanError: trunc,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got.Error, trunc) {
		t.Fatalf("the truncation must LEAD the plan's error: %q", got.Error)
	}
	if !strings.Contains(got.Error, "search: validate an iban: dial tcp") {
		t.Fatalf("per-step clauses must still follow it: %q", got.Error)
	}
	// It is attribution, not a verdict: the steps that DID plan still count.
	if got.StepCount != 2 || got.MatchedCount != 1 {
		t.Fatalf("truncation must not disturb the assembled steps: %+v", got)
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
