package nodes_test

import (
	"context"
	"strings"
	"testing"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

// cellAt places the current instance as a SearchStepAt cell at (col,row) in a
// column that already holds the authored row-0 cell.
func cellAt(t *testing.T, col, row int32) *fanoutTestContext {
	return &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, GridCol: col, GridRow: 0},
				{InstanceID: 3, Name: "SearchStepAt", PackageName: selfPkg, GridCol: col, GridRow: row},
			},
			current: 3,
		},
		mutation: &fanoutRecorder{},
	}
}

func cellPlan(queries ...string) *gen.FanOutPlan {
	return &gen.FanOutPlan{Queries: queries, FanoutCol: 3, ScoringMode: "lexical"}
}

// only returns the single CellResult the wrapper must always carry.
func only(t *testing.T, out *gen.FanOutCell) *gen.CellResult {
	t.Helper()
	if len(out.GetItems()) != 1 {
		t.Fatalf("the cell wrapper must carry EXACTLY one item (the join collects them with one repeated pick), got %d", len(out.GetItems()))
	}
	return out.GetItems()[0]
}

// The core contract: the cell searches EXACTLY the step at its own canvas row.
func TestSearchStepAt_SearchesOwnRowsStep(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"validate iban checksum": {apiRow("ValidateIban", "christiangeorgelucas/iban-tools", "0.2.0", "Validate an IBAN's checksum.")},
	})
	ax := cellAt(t, 3, 1)
	out, err := nodes.SearchStepAt(context.Background(), ax, cellPlan("render a summary card", "validate iban checksum", "fetch a url"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := only(t, out)
	if got.Row != 1 || got.Col != 3 || got.Query != "validate iban checksum" {
		t.Fatalf("cell must search its own row's step: %+v", got)
	}
	if !got.Matched || got.Score < 0.9 {
		t.Fatalf("on-domain candidate must match: %+v", got)
	}
	if got.Step.GetCandidates()[0].GetNode() != "ValidateIban" {
		t.Fatalf("wrong candidate: %+v", got.Step)
	}
	if got.Error != "" {
		t.Fatalf("clean run must carry no error: %q", got.Error)
	}
}

// The knob echo — the whole reason the output is a wrapper and not a bare
// CellResult. The assemble join's only inbound edges are cell edges, so
// threshold / max_alternatives / llm_error / task_blank can only reach it by
// riding the plan through the cell.
func TestSearchStepAt_EchoesTheBroadcastKnobs(t *testing.T) {
	fakeSearch(t, nil)
	ax := cellAt(t, 3, 0)
	p := cellPlan("parse a vcard")
	p.Threshold = 0.6
	p.MaxAlternatives = 2
	p.LlmError = "anthropic: 529 overloaded"
	p.TaskBlank = true
	p.PlanError = "TRUNCATED: 20 steps exceed the fan-out cap of 16; planning the first 16"

	out, err := nodes.SearchStepAt(context.Background(), ax, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.GetThreshold() != 0.6 || out.GetMaxAlternatives() != 2 {
		t.Fatalf("search knobs must be echoed for the join: %+v", out)
	}
	if out.GetLlmError() != "anthropic: 529 overloaded" || !out.GetTaskBlank() {
		t.Fatalf("attribution knobs must be echoed for the join: %+v", out)
	}
	if out.GetPlanError() != "TRUNCATED: 20 steps exceed the fan-out cap of 16; planning the first 16" {
		t.Fatalf("the caller-visible plan attribution must be echoed for the join: %+v", out)
	}
}

// Even a refusal must carry the wrapper and the echo — the join collects from
// EVERY member edge, so a cell that produced nothing still has to compose.
func TestSearchStepAt_EchoesKnobsOnTheRefusalPaths(t *testing.T) {
	ax := cellAt(t, 3, 0)
	out, err := nodes.SearchStepAt(context.Background(), ax, &gen.FanOutPlan{ScoringMode: "lexical", LlmError: "anthropic: no response", TaskBlank: true, Threshold: 0.7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(only(t, out).Error, "NO_QUERY") {
		t.Fatalf("empty plan must be NO_QUERY: %+v", out)
	}
	if out.GetLlmError() != "anthropic: no response" || !out.GetTaskBlank() || out.GetThreshold() != 0.7 {
		t.Fatalf("the echo must survive the NO_QUERY path: %+v", out)
	}
}

// A row past the plan's end is a clean, attributed error — no search.
func TestSearchStepAt_RowOutOfRange(t *testing.T) {
	fakeSearch(t, nil)
	ax := cellAt(t, 3, 5)
	out, err := nodes.SearchStepAt(context.Background(), ax, cellPlan("only step"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := only(t, out)
	if !strings.HasPrefix(got.Error, "ROW_OUT_OF_RANGE") || got.Matched || got.Query != "" {
		t.Fatalf("out-of-range row must be a clean error: %+v", got)
	}
}

// Direct invoke (no graph): searches row 0 and says so.
func TestSearchStepAt_NoGraphDefaultsToRowZero(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"parse a vcard": {apiRow("ParseVCard", "christiangeorgelucas/vcard-tools", "0.1.0", "Parse a vCard.")},
	})
	ax := &fanoutTestContext{t: t, reflection: fanoutReflection{}, mutation: &fanoutRecorder{}}
	out, err := nodes.SearchStepAt(context.Background(), ax, cellPlan("parse a vcard", "second step"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := only(t, out)
	if got.Row != 0 || got.Query != "parse a vcard" || !strings.Contains(got.Error, "NO_GRAPH") {
		t.Fatalf("no-graph invoke must search row 0 with a NO_GRAPH note: %+v", got)
	}
	if !got.Matched {
		t.Fatalf("row-0 candidate must match: %+v", got)
	}
}

// Below-threshold candidates are reported but not matched; the threshold knob
// forwards from the plan.
func TestSearchStepAt_ThresholdVerdict(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"look up bank details by code": {apiRow("GetPackaging", "dailymed-connector", "1.0.0", "Look up drug packaging details by product code.")},
	})
	ax := cellAt(t, 3, 0)
	got, err := nodes.SearchStepAt(context.Background(), ax, cellPlan("look up bank details by code"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := only(t, got)
	if c.Matched || c.Score >= 0.45 || len(c.Step.GetCandidates()) == 0 {
		t.Fatalf("generic-overlap candidate must be reported but unmatched: %+v", c)
	}

	p2 := cellPlan("look up bank details by code")
	p2.Threshold = 0.1
	got2, err := nodes.SearchStepAt(context.Background(), ax, p2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !only(t, got2).Matched {
		t.Fatalf("threshold knob must forward: %+v", got2)
	}
}

// The founder's layout change, at the cell: the column is authored at row 1,
// so the cell there owns step 0 — not queries[1]. Absolute indexing would have
// silently searched the wrong step for every cell in the column.
func TestSearchStepAt_StepIsRelativeToTheColumnBase(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"parse a vcard": {apiRow("ParseVCard", "christiangeorgelucas/vcard-tools", "0.1.2", "Parse a vCard.")},
	})
	// Column based at row 1, exactly as flow-planner-fanout@0.4.0 authors it.
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, GridCol: 3, GridRow: 1},
			},
			current: 2,
		},
		mutation: &fanoutRecorder{},
	}
	out, err := nodes.SearchStepAt(context.Background(), ax, cellPlan("parse a vcard", "second step", "third step"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := only(t, out)
	if got.Row != 1 {
		t.Fatalf("the cell must still report its CANVAS row: %+v", got)
	}
	if got.Query != "parse a vcard" {
		t.Fatalf("the column's base cell owns step 0 wherever it sits, got %q", got.Query)
	}
	if !got.Matched {
		t.Fatalf("the right step must actually have been searched: %+v", got)
	}
}

// Every row of a grown column, based at row 1: rows 1..3 own steps 0..2 in
// order. This is the shape a forked run produces after the layout change.
func TestSearchStepAt_EveryRowOfARebasedColumnOwnsItsOwnStep(t *testing.T) {
	queries := []string{"parse a vcard", "validate an iban", "render a qr code"}
	fakeSearch(t, map[string][]map[string]string{
		"parse a vcard":    {apiRow("ParseVCard", "christiangeorgelucas/vcard-tools", "0.1.2", "Parse a vCard.")},
		"validate an iban": {apiRow("ValidateIban", "christiangeorgelucas/iban-tools", "0.2.0", "Validate an IBAN.")},
		"render a qr code": {apiRow("EncodeQr", "christiangeorgelucas/barcode-tools", "0.3.0", "Render a QR code.")},
	})
	column := []axiom.ReflectionNode{
		{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, GridCol: 2, GridRow: 0},
		{InstanceID: 10, Name: "SearchStepAt", PackageName: selfPkg, GridCol: 3, GridRow: 1},
		{InstanceID: 11, Name: "SearchStepAt", PackageName: selfPkg, GridCol: 3, GridRow: 2},
		{InstanceID: 12, Name: "SearchStepAt", PackageName: selfPkg, GridCol: 3, GridRow: 3},
	}
	for i, inst := range []uint32{10, 11, 12} {
		ax := &fanoutTestContext{
			t:          t,
			reflection: fanoutReflection{nodes: column, current: inst},
			mutation:   &fanoutRecorder{},
		}
		out, err := nodes.SearchStepAt(context.Background(), ax, cellPlan(queries...))
		if err != nil {
			t.Fatalf("row %d: unexpected error: %v", i+1, err)
		}
		got := only(t, out)
		if got.Row != int32(i)+1 {
			t.Fatalf("cell %d reports the wrong canvas row: %d", i, got.Row)
		}
		if got.Query != queries[i] {
			t.Fatalf("row %d (step %d) must search %q, got %q", i+1, i, queries[i], got.Query)
		}
		if got.Error != "" {
			t.Fatalf("row %d: clean run must carry no error: %q", i+1, got.Error)
		}
	}
}
