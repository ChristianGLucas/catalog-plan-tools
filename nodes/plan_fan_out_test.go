package nodes_test

import (
	"context"
	"strings"
	"testing"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

const selfPkg = "christiangeorgelucas/catalog-plan-tools"

// fanoutTestContext is an axiom.Context whose reflection view and mutation
// sink are configurable — the shared testContext in assemble_plan_test.go
// serves nodes that never touch either surface.
type fanoutTestContext struct {
	t          *testing.T
	reflection fanoutReflection
	mutation   *fanoutRecorder
}

type fanoutReflection struct {
	nodes   []axiom.ReflectionNode
	edges   []axiom.ReflectionEdge
	current uint32
}

func (r fanoutReflection) Nodes() []axiom.ReflectionNode     { return r.nodes }
func (r fanoutReflection) Edges() []axiom.ReflectionEdge     { return r.edges }
func (r fanoutReflection) LoopEdges() []axiom.ReflectionEdge { return nil }
func (r fanoutReflection) Position() axiom.FlowPosition {
	return axiom.FlowPosition{CurrentInstance: r.current}
}
func (r fanoutReflection) GraphID() string { return "fanout-test-graph" }

type fanoutAdded struct {
	pkg, ver, node string
	pos            *axiom.CanvasPosition
}
type fanoutEdge struct{ src, dst uint32 }

type fanoutRecorder struct {
	nodes []fanoutAdded
	edges []fanoutEdge
}

func (m *fanoutRecorder) AddNode(pkg, ver, node string, pos *axiom.CanvasPosition) uint32 {
	m.nodes = append(m.nodes, fanoutAdded{pkg, ver, node, pos})
	return 100 + uint32(len(m.nodes)) - 1
}
func (m *fanoutRecorder) AddEdge(src, dst uint32, _ *axiom.EdgeCondition) {
	m.edges = append(m.edges, fanoutEdge{src, dst})
}
func (m *fanoutRecorder) Flow() axiom.FlowMutation { return m }

type fanoutLogger struct{ t *testing.T }

func (l fanoutLogger) Debug(msg string, args ...any) { l.t.Logf("DEBUG %s %v", msg, args) }
func (l fanoutLogger) Info(msg string, args ...any)  { l.t.Logf("INFO  %s %v", msg, args) }
func (l fanoutLogger) Warn(msg string, args ...any)  { l.t.Logf("WARN  %s %v", msg, args) }
func (l fanoutLogger) Error(msg string, args ...any) { l.t.Logf("ERROR %s %v", msg, args) }

type fanoutSecrets struct{}

func (fanoutSecrets) Get(string) (string, bool)        { return "", false }
func (fanoutSecrets) Status(string) axiom.SecretStatus { return axiom.SecretStatusUnset }

func (c *fanoutTestContext) Log() axiom.Logger            { return fanoutLogger{c.t} }
func (c *fanoutTestContext) Secrets() axiom.Secrets       { return fanoutSecrets{} }
func (c *fanoutTestContext) ExecutionID() string          { return "fanout-test-exec" }
func (c *fanoutTestContext) FlowID() string               { return "fanout-test-flow" }
func (c *fanoutTestContext) TenantID() string             { return "fanout-test-tenant" }
func (c *fanoutTestContext) Reflection() axiom.Reflection { return c }
func (c *fanoutTestContext) Flow() axiom.FlowReflection   { return c.reflection }
func (c *fanoutTestContext) Mutation() axiom.Mutation     { return c.mutation }

var (
	_ axiom.Context    = (*fanoutTestContext)(nil)
	_ axiom.Reflection = (*fanoutTestContext)(nil)
)

// plannerGraph mirrors the authored five-column flow: decompose (col 1),
// PlanFanOut (col 2, the emitter and current instance), the ONE authored
// SearchStepAt cell (col 3 row 0), AssembleFromCells (col 4) and CheckBridges
// (col 5) — with the cell wired into the join. extra nodes/edges let a test
// perturb that shape.
func plannerGraph(extraNodes []axiom.ReflectionNode, extraEdges []axiom.ReflectionEdge) fanoutReflection {
	nodes := []axiom.ReflectionNode{
		{InstanceID: 0, Name: "CreateMessage", PackageName: "christiangeorgelucas/anthropic-connector", GridCol: 1, GridRow: 0},
		{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 2, GridRow: 0},
		{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 3, GridRow: 0},
		{InstanceID: 3, Name: "AssembleFromCells", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 4, GridRow: 0},
		{InstanceID: 4, Name: "CheckBridges", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 5, GridRow: 0},
	}
	edges := []axiom.ReflectionEdge{
		{SrcInstance: 0, DstInstance: 1},
		{SrcInstance: 1, DstInstance: 2},
		{SrcInstance: 2, DstInstance: 3},
		{SrcInstance: 3, DstInstance: 4},
	}
	return fanoutReflection{
		nodes:   append(nodes, extraNodes...),
		edges:   append(edges, extraEdges...),
		current: 1,
	}
}

func plannerContext(t *testing.T) *fanoutTestContext {
	return &fanoutTestContext{t: t, reflection: plannerGraph(nil, nil), mutation: &fanoutRecorder{}}
}

// The core contract: N steps grow the AUTHORED cell column to N rows — the
// authored cell keeps row 0 and mutation appends rows 1..N-1 — and every
// appended cell is wired BOTH ways: the plan broadcast in from the emitter,
// and the join membership out into the authored assemble node.
func TestPlanFanOut_AppendsCellsIntoTheAuthoredColumn(t *testing.T) {
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"validate iban checksum", "look up bank identity", "render summary card"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Mutated || got.Appended != 2 || got.Error != "" {
		t.Fatalf("want mutated=true appended=2 (3 steps, one authored) no error, got %+v", got)
	}
	if got.FanoutCol != 3 {
		t.Fatalf("the fan-out column must be DISCOVERED from the authored cell (3), got %d", got.FanoutCol)
	}
	m := ax.mutation
	if len(m.nodes) != 2 || len(m.edges) != 4 {
		t.Fatalf("want 2 cells + 4 edges (in+out per cell), got %d/%d", len(m.nodes), len(m.edges))
	}
	for i, n := range m.nodes {
		row := int32(i + 1)
		if n.pkg != selfPkg || n.ver != "0.6.0" || n.node != "SearchStepAt" {
			t.Fatalf("cell %d must be THIS package's SearchStepAt, got %s@%s/%s", i, n.pkg, n.ver, n.node)
		}
		if n.pos == nil || n.pos.GridCol != 3 || n.pos.GridRow != row {
			t.Fatalf("cell %d must land at (3,%d), got %+v", i, row, n.pos)
		}
		cellInstance := 100 + uint32(i)
		if m.edges[2*i] != (fanoutEdge{1, cellInstance}) {
			t.Fatalf("cell %d missing the emitter->cell broadcast edge: %+v", i, m.edges[2*i])
		}
		if m.edges[2*i+1] != (fanoutEdge{cellInstance, 3}) {
			t.Fatalf("cell %d missing the cell->assemble join edge: %+v", i, m.edges[2*i+1])
		}
	}
}

// A single-step plan needs no growth at all: the authored graph already fits
// it, so the run must complete WITHOUT forking.
func TestPlanFanOut_SingleStepDoesNotMutate(t *testing.T) {
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"validate iban checksum"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || got.Appended != 0 || got.Error != "" {
		t.Fatalf("a one-step plan must not mutate and must not complain: %+v", got)
	}
	if len(ax.mutation.nodes) != 0 || len(ax.mutation.edges) != 0 {
		t.Fatalf("no mutation may be emitted: %d nodes, %d edges", len(ax.mutation.nodes), len(ax.mutation.edges))
	}
	if got.FanoutCol != 3 || len(got.Queries) != 1 {
		t.Fatalf("the plan must still be reported for the authored cell: %+v", got)
	}
}

// The idempotence guard: the forked child re-runs the emitter, and its column
// already holds every cell the plan needs. Never append twice.
func TestPlanFanOut_AlreadyGrownIsAnAttributedNoOp(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: plannerGraph(
			[]axiom.ReflectionNode{
				{InstanceID: 5, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 3, GridRow: 1},
			},
			[]axiom.ReflectionEdge{{SrcInstance: 1, DstInstance: 5}, {SrcInstance: 5, DstInstance: 3}},
		),
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"a b c", "d e f"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || got.Appended != 0 || len(ax.mutation.nodes) != 0 {
		t.Fatalf("guard failed: %+v (added %d)", got, len(ax.mutation.nodes))
	}
	if !strings.HasPrefix(got.Error, "ALREADY_GROWN") {
		t.Fatalf("the guard no-op must be attributed, got error %q", got.Error)
	}
	if got.FanoutCol != 3 {
		t.Fatalf("the observed column must still be reported: %+v", got)
	}
}

// A flow with no authored cell of this package cannot be grown — refuse
// loudly rather than invent a column.
func TestPlanFanOut_NoAuthoredCellRefuses(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchSteps", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 3, GridRow: 0},
			},
			current: 1,
		},
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "NO_CELL") {
		t.Fatalf("a flow with no cell must refuse with NO_CELL: %+v", got)
	}
}

// A SearchStepAt from ANOTHER package is not this emitter's template: the
// column is only ever grown with cells the emitter itself can vouch for.
func TestPlanFanOut_ForeignPackageCellIsNotATemplate(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: "someoneelse/lookalike-tools", PackageVersion: "9.9.9", GridCol: 3, GridRow: 0},
			},
			edges:   []axiom.ReflectionEdge{{SrcInstance: 2, DstInstance: 1}},
			current: 1,
		},
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "NO_CELL") {
		t.Fatalf("a foreign SearchStepAt must not be adopted as the template: %+v", got)
	}
}

// Mutation never ADDS the join — it is authored from the start. A cell that
// feeds nothing has no join to grow, so refuse rather than mint a second
// terminal (which the platform would reject anyway).
func TestPlanFanOut_CellWithoutJoinRefuses(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 3, GridRow: 0},
			},
			edges:   []axiom.ReflectionEdge{{SrcInstance: 1, DstInstance: 2}},
			current: 1,
		},
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "NO_JOIN") {
		t.Fatalf("a dangling cell must refuse with NO_JOIN: %+v", got)
	}
}

// Cells select their step by canvas row, so an authored cell off row 0 would
// shift every step index. Refuse rather than plan a misaligned fan-out.
func TestPlanFanOut_AuthoredCellMustStartAtRowZero(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 3, GridRow: 2},
				{InstanceID: 3, Name: "AssembleFromCells", PackageName: selfPkg, PackageVersion: "0.6.0", GridCol: 4, GridRow: 0},
			},
			edges:   []axiom.ReflectionEdge{{SrcInstance: 1, DstInstance: 2}, {SrcInstance: 2, DstInstance: 3}},
			current: 1,
		},
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "CELL_ROW_NOT_ZERO") {
		t.Fatalf("an off-row-0 cell column must refuse: %+v", got)
	}
}

// R18 MINOR regressions: multi-clause errors join with "; ", and an upstream
// LLM failure is attributed on the NO_QUERY path.
func TestPlanFanOut_ErrorSeparatorAndLLMAttribution(t *testing.T) {
	qs := make([]string, 20)
	for i := range qs {
		qs[i] = "q" + strings.Repeat("x", i)
	}
	ax := &fanoutTestContext{t: t, reflection: fanoutReflection{}, mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: qs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got.Error, "; NO_GRAPH") || !strings.HasPrefix(got.Error, "TRUNCATED") {
		t.Fatalf("clauses must join with '; ': %q", got.Error)
	}

	ax2 := plannerContext(t)
	got2, err := nodes.PlanFanOut(context.Background(), ax2,
		&gen.FanOutRequest{QueriesJson: "the model was unavailable", LlmError: "anthropic: 529 overloaded"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got2.Error, "decompose: anthropic: 529 overloaded; NO_QUERY") {
		t.Fatalf("LLM failure must be attributed: %q", got2.Error)
	}
}

// The broadcast knobs must ride the PLAN into the cells: the assemble join's
// only inbound edges are cell edges, so anything it needs travels this way.
func TestPlanFanOut_EchoesBroadcastKnobs(t *testing.T) {
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{
		Queries:         []string{"parse a vcard", "validate an iban"},
		MaxAlternatives: 2,
		Threshold:       0.6,
		LlmError:        "anthropic: truncated response",
		Task:            "verify an IBAN and look up its bank",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MaxAlternatives != 2 || got.Threshold != 0.6 {
		t.Fatalf("search knobs must forward to the cells: %+v", got)
	}
	if got.LlmError != "anthropic: truncated response" {
		t.Fatalf("llm_error must ride the plan to the cells: %q", got.LlmError)
	}
	if got.TaskBlank {
		t.Fatalf("task_blank must echo the request, got true")
	}
	// The forked child resumes AT this node and @flow_input never re-fires
	// there, so the task text can only reach the terminal from here.
	if got.Task != "verify an IBAN and look up its bank" {
		t.Fatalf("the task text must be echoed for nodes downstream of the fork: %q", got.Task)
	}
}

// Every exit path must echo the task carrier — a refusal still runs the rest
// of the authored graph, and the terminal still needs the task text.
func TestPlanFanOut_TaskCarrierSurvivesEveryExitPath(t *testing.T) {
	const task = "verify an IBAN and look up its bank"
	cases := map[string]*gen.FanOutRequest{
		"blank task": {Queries: []string{"a b", "c d"}, TaskBlank: true, Task: task},
		"no query":   {Task: task},
		"grown":      {Queries: []string{"a b", "c d"}, Task: task},
		"one step":   {Queries: []string{"a b"}, Task: task},
	}
	for name, req := range cases {
		ax := plannerContext(t)
		got, err := nodes.PlanFanOut(context.Background(), ax, req)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if got.Task != task {
			t.Fatalf("%s: the task carrier must survive: %q", name, got.Task)
		}
	}
}

// queries_json path uses the shared defensive parser; blank input is a clean
// NO_QUERY with no mutation.
func TestPlanFanOut_QueriesJSONAndBlank(t *testing.T) {
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{QueriesJson: "```json\n{\"steps\":[{\"query\":\"fetch a url\"},{\"query\":\"hash the body\"}]}\n```"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Appended != 1 || len(got.Queries) != 2 || got.Queries[1] != "hash the body" {
		t.Fatalf("queries_json parse failed: %+v", got)
	}

	ax2 := plannerContext(t)
	got2, err := nodes.PlanFanOut(context.Background(), ax2, &gen.FanOutRequest{Query: "   "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got2.Error, "NO_QUERY") || got2.Mutated || len(ax2.mutation.nodes) != 0 {
		t.Fatalf("blank input must be a clean NO_QUERY with no mutation: %+v", got2)
	}
}

// Standalone direct-invoke (no graph): parse and report, never mutate.
func TestPlanFanOut_NoGraphIsSafeNoOp(t *testing.T) {
	ax := &fanoutTestContext{t: t, reflection: fanoutReflection{}, mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"x y"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.Contains(got.Error, "NO_GRAPH") {
		t.Fatalf("no-graph invoke must be a reported no-op: %+v", got)
	}
}

// The defensive cap: a degenerate step list cannot mint an absurd batch.
func TestPlanFanOut_CapTruncates(t *testing.T) {
	qs := make([]string, 40)
	for i := range qs {
		qs[i] = "q" + strings.Repeat("x", i)
	}
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: qs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Queries) != 16 || got.Appended != 15 || len(ax.mutation.nodes) != 15 || !strings.HasPrefix(got.Error, "TRUNCATED") {
		t.Fatalf("cap must truncate to 16 steps (15 appended) with attribution: appended=%d err=%q", got.Appended, got.Error)
	}
	// R19 MINOR-2: truncation changes the SHAPE of the plan the caller reads,
	// so it must travel the caller-visible channel too — this node's own error
	// field has no route out of the graph.
	if !strings.HasPrefix(got.PlanError, "TRUNCATED") {
		t.Fatalf("TRUNCATED must reach the caller via plan_error, got %q", got.PlanError)
	}
}

// The counterpart to the above: bookkeeping clauses must NOT reach the caller.
// ALREADY_GROWN fires on every forked child, so surfacing it would put noise in
// the error field of every successful multi-step plan.
func TestPlanFanOut_InternalClausesStayOffTheCallerChannel(t *testing.T) {
	grown := &fanoutTestContext{
		t: t,
		reflection: plannerGraph(
			[]axiom.ReflectionNode{
				{InstanceID: 5, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 3, GridRow: 1},
			},
			[]axiom.ReflectionEdge{{SrcInstance: 1, DstInstance: 5}, {SrcInstance: 5, DstInstance: 3}},
		),
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), grown, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got.Error, "ALREADY_GROWN") || got.PlanError != "" {
		t.Fatalf("ALREADY_GROWN must stay internal: error=%q plan_error=%q", got.Error, got.PlanError)
	}

	noGraph := &fanoutTestContext{t: t, reflection: fanoutReflection{}, mutation: &fanoutRecorder{}}
	got2, err := nodes.PlanFanOut(context.Background(), noGraph, &gen.FanOutRequest{Queries: []string{"x y"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got2.Error, "NO_GRAPH") || got2.PlanError != "" {
		t.Fatalf("NO_GRAPH must stay internal: error=%q plan_error=%q", got2.Error, got2.PlanError)
	}
}

// R19 MINOR-6: a column whose lowest cell is off row 0 is MIS-AUTHORED. Before
// the fix the grown guard ran first and blamed the lineage (ALREADY_GROWN) for
// what is an authoring fault.
func TestPlanFanOut_MisalignedColumnIsNotReportedAsAlreadyGrown(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutReflection{
			nodes: []axiom.ReflectionNode{
				{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 2, GridRow: 0},
				{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 3, GridRow: 1},
				{InstanceID: 5, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 3, GridRow: 2},
				{InstanceID: 3, Name: "AssembleFromCells", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 4, GridRow: 0},
			},
			edges: []axiom.ReflectionEdge{
				{SrcInstance: 1, DstInstance: 2}, {SrcInstance: 2, DstInstance: 3},
				{SrcInstance: 1, DstInstance: 5}, {SrcInstance: 5, DstInstance: 3},
			},
			current: 1,
		},
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 {
		t.Fatalf("a misaligned column must not be grown: %+v", got)
	}
	if !strings.HasPrefix(got.Error, "CELL_ROW_NOT_ZERO") {
		t.Fatalf("misalignment must be attributed as CELL_ROW_NOT_ZERO, got %q", got.Error)
	}
	// R19 MINOR-7: refusing to grow is not enough. The misaligned cell still
	// receives the plan and still feeds the join, so the caller would get a
	// one-step plan of the WRONG step labelled plan_basis "decomposed". That
	// has to reach them.
	if !strings.HasPrefix(got.PlanError, "CELL_ROW_NOT_ZERO") {
		t.Fatalf("CELL_ROW_NOT_ZERO must reach the caller via plan_error, got %q", got.PlanError)
	}
}

// The other side of the R19 MINOR-7 split: NO_CELL and NO_JOIN stay internal,
// because in those shapes there is no caller to inform — NO_CELL means there is
// no cell to echo through, NO_JOIN means the cell feeds nothing, so no plan is
// assembled for anyone to be misled by.
func TestPlanFanOut_ShapesWithNoEchoChannelStayInternal(t *testing.T) {
	planFanOutNode := axiom.ReflectionNode{InstanceID: 1, Name: "PlanFanOut", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 2, GridRow: 0}
	cellNode := axiom.ReflectionNode{InstanceID: 2, Name: "SearchStepAt", PackageName: selfPkg, PackageVersion: "0.6.2", GridCol: 3, GridRow: 0}

	cases := map[string]fanoutReflection{
		// No SearchStepAt anywhere: nothing to echo through.
		"NO_CELL": {
			nodes:   []axiom.ReflectionNode{planFanOutNode, {InstanceID: 2, Name: "SearchSteps", PackageName: selfPkg, GridCol: 3, GridRow: 0}},
			current: 1,
		},
		// A cell that feeds nothing: no join, so no plan is assembled.
		"NO_JOIN": {
			nodes:   []axiom.ReflectionNode{planFanOutNode, cellNode},
			edges:   []axiom.ReflectionEdge{{SrcInstance: 1, DstInstance: 2}},
			current: 1,
		},
	}
	for want, graph := range cases {
		ax := &fanoutTestContext{t: t, reflection: graph, mutation: &fanoutRecorder{}}
		got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b", "c d"}})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", want, err)
		}
		if !strings.HasPrefix(got.Error, want) {
			t.Fatalf("%s: wrong attribution: %q", want, got.Error)
		}
		if got.PlanError != "" {
			t.Fatalf("%s: has no echo channel, so it must not claim the caller-visible one: %q", want, got.PlanError)
		}
	}
}

// The v0 phantom-plan guard: a blank task must refuse to fan out even when
// upstream invented steps for it.
func TestPlanFanOut_BlankTaskRefusesToFanOut(t *testing.T) {
	ax := plannerContext(t)
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"invented step one", "invented step two"}, TaskBlank: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "BLANK_TASK") {
		t.Fatalf("blank task must refuse to fan out: %+v", got)
	}
	// The blank verdict must still ride the plan to the authored cell, so the
	// join's assemble step reaches the same NO_INPUT verdict the sync flow does.
	if !got.TaskBlank {
		t.Fatalf("task_blank must ride the plan even on the refusal path: %+v", got)
	}
}
