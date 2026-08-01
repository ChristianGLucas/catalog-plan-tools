package nodes

import (
	"context"
	"fmt"
	"strings"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

const (
	// defaultFanoutCol is the dedicated canvas column the appended search
	// cells land in when the request does not name one. The cells' column is
	// reserved for the fan-out: it grows by appending one row per step, and
	// the reflection guard below keys on it.
	defaultFanoutCol = 4
	// cellPackage/cellVersion pin the appended search cell. AddNode resolves
	// the target package's single mutation-capable node (SearchStepAt), whose
	// input message mirrors FanOutPlan field-for-field — mutation-added edges
	// carry no adapters, so the wire contract is this exact message.
	cellPackage = "christiangeorgelucas/plan-fanout-cell"
	cellVersion = "0.1.0"
	// maxFanout caps how many cells one invocation may append — a defensive
	// ceiling far above what a 2-6 step decomposition produces, so a
	// degenerate queries list cannot mint an absurd mutation batch.
	maxFanout = 16
)

// Grows the running flow to fit the plan: given the decomposed steps, appends one search cell
// per step into a dedicated fan-out canvas column (one row per step, so the column expands
// indefinitely as plans get longer) and wires itself to every cell — the whole plan is
// broadcast and each cell selects its own step by reading its canvas row via reflection.
// The append happens at most once per lineage: when the fan-out column is already populated,
// or the node runs outside a flow, it only reports what it observed. The invoking execution
// terminates FORKED with no payload BY DESIGN — the per-step results live on the forked child
// execution (each cell's output, visible on the lineage canvas and via the executions API).
func PlanFanOut(ctx context.Context, ax axiom.Context, input *gen.FanOutRequest) (*gen.FanOutPlan, error) {
	out := &gen.FanOutPlan{
		FanoutCol:       input.GetFanoutCol(),
		MaxAlternatives: input.GetMaxAlternatives(),
		Threshold:       input.GetThreshold(),
	}
	if out.FanoutCol == 0 {
		out.FanoutCol = defaultFanoutCol
	}

	// Resolve the queries with SearchSteps' exact precedence and defensive
	// JSON parsing, so the flow can feed raw LLM text straight in.
	switch {
	case len(input.GetQueries()) > 0:
		for _, q := range input.GetQueries() {
			if s := strings.TrimSpace(q); s != "" {
				out.Queries = append(out.Queries, s)
			}
		}
	case strings.TrimSpace(input.GetQueriesJson()) != "":
		qs, err := parseQueriesJSON(input.GetQueriesJson())
		if err != nil {
			out.Error = "NO_QUERY: " + err.Error()
			return out, nil
		}
		out.Queries = qs
	case strings.TrimSpace(input.GetQuery()) != "":
		out.Queries = []string{strings.TrimSpace(input.GetQuery())}
	}
	if len(out.Queries) == 0 {
		out.Error = "NO_QUERY: no usable query in queries, queries_json, or query"
		return out, nil
	}
	if len(out.Queries) > maxFanout {
		out.Error = fmt.Sprintf("TRUNCATED: %d steps exceed the fan-out cap of %d; planning the first %d", len(out.Queries), maxFanout, maxFanout)
		out.Queries = out.Queries[:maxFanout]
	}

	fr := ax.Reflection().Flow()
	nodes := fr.Nodes()
	if len(nodes) == 0 {
		// Standalone direct-invoke: no running graph to grow. Report the
		// parsed plan so the node stays testable outside a flow.
		out.Error = strings.TrimSpace(out.Error + " NO_GRAPH: not running inside a flow; nothing appended")
		return out, nil
	}
	for _, n := range nodes {
		if n.GridCol == out.FanoutCol {
			out.Appended = 0
			// Idempotence guard: the fan-out column is already populated —
			// this is a re-run on an already-grown lineage. Never append twice.
			return out, nil
		}
	}

	self := fr.Position().CurrentInstance
	fm := ax.Mutation().Flow()
	for k := range out.Queries {
		cell := fm.AddNode(cellPackage, cellVersion, &axiom.CanvasPosition{GridCol: out.FanoutCol, GridRow: int32(k)})
		fm.AddEdge(self, cell, nil)
	}
	out.Mutated = true
	out.Appended = int32(len(out.Queries))
	return out, nil
}
