package nodes_test

import (
	"context"
	"strings"
	"testing"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

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
	current uint32
}

func (r fanoutReflection) Nodes() []axiom.ReflectionNode     { return r.nodes }
func (r fanoutReflection) Edges() []axiom.ReflectionEdge     { return nil }
func (r fanoutReflection) LoopEdges() []axiom.ReflectionEdge { return nil }
func (r fanoutReflection) Position() axiom.FlowPosition {
	return axiom.FlowPosition{CurrentInstance: r.current}
}
func (r fanoutReflection) GraphID() string { return "fanout-test-graph" }

type fanoutAdded struct {
	pkg, ver string
	pos      *axiom.CanvasPosition
}
type fanoutEdge struct{ src, dst uint32 }

type fanoutRecorder struct {
	nodes []fanoutAdded
	edges []fanoutEdge
}

func (m *fanoutRecorder) AddNode(pkg, ver string, pos *axiom.CanvasPosition) uint32 {
	m.nodes = append(m.nodes, fanoutAdded{pkg, ver, pos})
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

func fanoutGraph(current uint32, extra ...axiom.ReflectionNode) fanoutReflection {
	base := []axiom.ReflectionNode{
		{InstanceID: 0, Name: "decompose", GridCol: 1, GridRow: 0},
		{InstanceID: 1, Name: "PlanFanOut", PackageName: "christiangeorgelucas/catalog-plan-tools", GridCol: 2, GridRow: 0},
	}
	return fanoutReflection{nodes: append(base, extra...), current: current}
}

// The core contract: one cell per step, all in the SAME dedicated column,
// rows 0..N-1 in step order, every cell wired from the emitter.
func TestPlanFanOut_AppendsOneCellPerStepInDedicatedColumn(t *testing.T) {
	ax := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"validate iban checksum", "look up bank identity", "render summary card"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Mutated || got.Appended != 3 || got.Error != "" {
		t.Fatalf("want mutated=true appended=3 no error, got %+v", got)
	}
	if got.FanoutCol != 4 {
		t.Fatalf("default fan-out column must be 4, got %d", got.FanoutCol)
	}
	m := ax.mutation
	if len(m.nodes) != 3 || len(m.edges) != 3 {
		t.Fatalf("want 3 cells + 3 edges, got %d/%d", len(m.nodes), len(m.edges))
	}
	for k, n := range m.nodes {
		if n.pkg != "christiangeorgelucas/plan-fanout-cell" || n.ver != "0.1.0" {
			t.Fatalf("cell %d wrong package: %s@%s", k, n.pkg, n.ver)
		}
		if n.pos == nil || n.pos.GridCol != 4 || n.pos.GridRow != int32(k) {
			t.Fatalf("cell %d must land at (4,%d), got %+v", k, k, n.pos)
		}
		if m.edges[k].src != 1 || m.edges[k].dst != 100+uint32(k) {
			t.Fatalf("edge %d mis-wired: %+v", k, m.edges[k])
		}
	}
}

// A populated fan-out column means an already-grown lineage: never append twice.
func TestPlanFanOut_GuardOnPopulatedColumn(t *testing.T) {
	ax := &fanoutTestContext{
		t: t,
		reflection: fanoutGraph(1,
			axiom.ReflectionNode{InstanceID: 2, Name: "SearchStepAt", GridCol: 4, GridRow: 0},
		),
		mutation: &fanoutRecorder{},
	}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: []string{"a b c"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || got.Appended != 0 || len(ax.mutation.nodes) != 0 {
		t.Fatalf("guard failed: %+v (added %d)", got, len(ax.mutation.nodes))
	}
}

// Custom column honored end to end (placement + guard key), knobs forwarded.
func TestPlanFanOut_CustomColumn(t *testing.T) {
	ax := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"parse a vcard"}, FanoutCol: 7, MaxAlternatives: 2, Threshold: 0.6})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.FanoutCol != 7 || ax.mutation.nodes[0].pos.GridCol != 7 {
		t.Fatalf("custom column not honored: %+v", ax.mutation.nodes)
	}
	if got.MaxAlternatives != 2 || got.Threshold != 0.6 {
		t.Fatalf("knobs must forward to the cells: %+v", got)
	}
}

// queries_json path uses the shared defensive parser; blank input is a clean
// NO_QUERY with no mutation.
func TestPlanFanOut_QueriesJSONAndBlank(t *testing.T) {
	ax := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{QueriesJson: "```json\n{\"steps\":[{\"query\":\"fetch a url\"},{\"query\":\"hash the body\"}]}\n```"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Appended != 2 || len(got.Queries) != 2 || got.Queries[1] != "hash the body" {
		t.Fatalf("queries_json parse failed: %+v", got)
	}

	ax2 := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
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
		qs[i] = "q"
	}
	ax := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax, &gen.FanOutRequest{Queries: qs})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Appended != 16 || len(ax.mutation.nodes) != 16 || !strings.HasPrefix(got.Error, "TRUNCATED") {
		t.Fatalf("cap must truncate to 16 with attribution: appended=%d err=%q", got.Appended, got.Error)
	}
}

// The v0 phantom-plan guard: a blank task must refuse to fan out even when
// upstream invented steps for it.
func TestPlanFanOut_BlankTaskRefusesToFanOut(t *testing.T) {
	ax := &fanoutTestContext{t: t, reflection: fanoutGraph(1), mutation: &fanoutRecorder{}}
	got, err := nodes.PlanFanOut(context.Background(), ax,
		&gen.FanOutRequest{Queries: []string{"invented step one", "invented step two"}, TaskBlank: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Mutated || len(ax.mutation.nodes) != 0 || !strings.HasPrefix(got.Error, "BLANK_TASK") {
		t.Fatalf("blank task must refuse to fan out: %+v", got)
	}
}
