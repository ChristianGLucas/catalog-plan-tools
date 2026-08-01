package nodes

import (
	"context"
	"fmt"
	"strings"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

const (
	// cellNodeName is the node PlanFanOut appends — SearchStepAt, from THIS
	// package. The flow authors row 0 of the cell column; mutation appends
	// rows 1..N-1 of the same column, so the template is found by reflection
	// rather than named by a caller knob.
	cellNodeName = "SearchStepAt"
	// maxFanout caps how many cells one invocation may append — a defensive
	// ceiling far above what a 2-6 step decomposition produces, so a
	// degenerate queries list cannot mint an absurd mutation batch.
	maxFanout = 16
)

// PlanFanOut grows the running planner flow to fit the decomposed task: given the steps, it locates the flow's AUTHORED search cell (this package's SearchStepAt) by reflection and appends one more cell per additional step into that same column — row k searches step k — wiring each new cell exactly like the authored one (the plan broadcast in, the assemble join out), so the join's membership grows with the plan. A single-step plan needs no growth at all: the authored graph already fits and the run completes without mutating. A multi-step plan emits one mutation batch and the run continues on the forked child graph, whose terminal result — the full assembled plan — is what the caller receives. The append happens at most once per lineage (an already-grown graph only reports, ALREADY_GROWN), and every refusal is attributed in the error field: BLANK_TASK for a task the caller knows was blank (an LLM handed an empty task invents steps, and here each invented step would mint a real node), NO_QUERY when nothing usable was passed, NO_CELL / NO_JOIN when the flow has no authored cell or the cell feeds nothing, NO_GRAPH outside a flow, TRUNCATED past the fan-out cap. Every broadcast knob the downstream join needs (threshold, max_alternatives, llm_error, task_blank) and the original task text are echoed onto the plan, because a mutating run's forked child resumes AT this node and the flow's own input node does not re-fire there — an edge out of this node is the only path to anything downstream of the fork. Reads only the running graph — no network, no secrets.
func PlanFanOut(ctx context.Context, ax axiom.Context, input *gen.FanOutRequest) (*gen.FanOutPlan, error) {
	out := &gen.FanOutPlan{
		MaxAlternatives: input.GetMaxAlternatives(),
		Threshold:       input.GetThreshold(),
		LlmError:        strings.TrimSpace(input.GetLlmError()),
		TaskBlank:       input.GetTaskBlank(),
		Task:            input.GetTask(),
	}

	// Refuse to fan out for a task the caller KNOWS was blank — an LLM
	// decomposer handed an empty task cheerfully invents generic steps
	// (the phantom-plan class found live in v0), and here each invented
	// step would mint a real appended node.
	if input.GetTaskBlank() {
		out.Error = "BLANK_TASK: the original task text was blank; refusing to fan out over invented steps"
		return out, nil
	}

	// Resolve the queries with SearchSteps' exact precedence and defensive
	// JSON parsing, so the flow can feed raw LLM text straight in.
	switch {
	case len(input.GetQueries()) > 0:
		out.Queries = nonBlank(input.GetQueries())
	case strings.TrimSpace(input.GetQueriesJson()) != "":
		qs, err := parseQueriesJSON(input.GetQueriesJson())
		if err != nil {
			out.Error = "NO_QUERY: " + err.Error()
			if out.LlmError != "" {
				out.Error = "decompose: " + out.LlmError + "; " + out.Error
			}
			return out, nil
		}
		out.Queries = qs
	case strings.TrimSpace(input.GetQuery()) != "":
		out.Queries = []string{strings.TrimSpace(input.GetQuery())}
	}
	if len(out.Queries) == 0 {
		out.Error = "NO_QUERY: no usable query in queries, queries_json, or query"
		if out.LlmError != "" {
			out.Error = "decompose: " + out.LlmError + "; " + out.Error
		}
		return out, nil
	}
	if len(out.Queries) > maxFanout {
		// Shape-changing: the caller is about to read a plan SHORTER than the
		// task decomposed to, so this clause must travel all the way out —
		// appendPlanError puts it on the caller-visible channel too.
		appendPlanError(out, fmt.Sprintf("TRUNCATED: %d steps exceed the fan-out cap of %d; planning the first %d", len(out.Queries), maxFanout, maxFanout))
		out.Queries = out.Queries[:maxFanout]
	}

	fr := ax.Reflection().Flow()
	nodes := fr.Nodes()
	if len(nodes) == 0 {
		// Standalone direct-invoke: no running graph to grow. Report the
		// parsed plan so the node stays testable outside a flow.
		appendError(out, "NO_GRAPH: not running inside a flow; nothing appended")
		return out, nil
	}
	self := fr.Position().CurrentInstance

	// Locate the template cell: this package's authored SearchStepAt, lowest
	// row. Its column IS the fan-out column — the caller never picks one, so
	// a column collision with an authored node is impossible by construction.
	var selfPkg, selfVer string
	var template *axiom.ReflectionNode
	cells := 0
	for _, n := range nodes {
		if n.InstanceID == self {
			selfPkg, selfVer = n.PackageName, n.PackageVersion
		}
	}
	if selfPkg == "" {
		appendError(out, fmt.Sprintf("NO_POSITION: instance %d is not in the reflection view; nothing appended", self))
		return out, nil
	}
	for i, n := range nodes {
		if n.Name != cellNodeName || n.PackageName != selfPkg {
			continue
		}
		cells++
		if template == nil || n.GridRow < template.GridRow {
			template = &nodes[i]
		}
	}
	if template == nil {
		appendError(out, fmt.Sprintf("NO_CELL: this flow has no authored %s cell from %s to extend", cellNodeName, selfPkg))
		return out, nil
	}
	out.FanoutCol = template.GridCol
	// Row validity is checked BEFORE the grown guard: a column whose lowest
	// cell is off row 0 is MIS-AUTHORED, and reporting it as ALREADY_GROWN
	// would blame the lineage for an authoring fault (R19 MINOR-6). A genuinely
	// grown lineage always has its authored cell at row 0, so this never
	// shadows ALREADY_GROWN in practice.
	if template.GridRow != 0 {
		// Cells select their step by canvas row, so the authored cell must sit
		// at row 0 or every step index would be shifted. Refusing to grow is
		// not enough: the misaligned cell still receives the plan, still
		// echoes, and still feeds the join, so the caller would get a ONE-step
		// plan built from the wrong step (or a ROW_OUT_OF_RANGE stub) labelled
		// plan_basis "decomposed" — indistinguishable from a genuine one-step
		// task. A 1-of-N plan is a bigger distortion than TRUNCATED's 16-of-20,
		// so it goes on the caller-visible channel (R19 MINOR-7).
		appendPlanError(out, fmt.Sprintf("CELL_ROW_NOT_ZERO: the lowest authored %s cell sits at row %d; a fan-out column must start at row 0", cellNodeName, template.GridRow))
		return out, nil
	}
	if cells > 1 {
		// The idempotence guard: a grown lineage re-runs the emitter (the
		// child resumes from it), and the column already holds every cell the
		// plan needs. Report rather than append again.
		appendError(out, fmt.Sprintf("ALREADY_GROWN: the fan-out column %d already holds %d cells; not appending again", out.FanoutCol, cells))
		return out, nil
	}

	// The join the cells feed: the destination of the template cell's
	// out-edge. Mutation never ADDS the join (it is authored from the start);
	// it only adds members to it.
	var assembleInstance uint32
	haveJoin := false
	for _, e := range fr.Edges() {
		if e.SrcInstance == template.InstanceID {
			assembleInstance = e.DstInstance
			haveJoin = true
			break
		}
	}
	if !haveJoin {
		appendError(out, fmt.Sprintf("NO_JOIN: the authored %s cell has no downstream edge to grow the join over", cellNodeName))
		return out, nil
	}

	if len(out.Queries) == 1 {
		// Single-step plan: the authored graph already fits it exactly.
		// No mutation, no fork — an ordinary request/response run.
		return out, nil
	}

	fm := ax.Mutation().Flow()
	for k := 1; k < len(out.Queries); k++ {
		cell := fm.AddNode(selfPkg, selfVer, cellNodeName, &axiom.CanvasPosition{GridCol: out.FanoutCol, GridRow: int32(k)})
		// Both edges replicate the authored template row: the broadcast in,
		// and the join membership out. Every appended cell MUST be wired
		// forward — the platform rejects a batch that would leave the child
		// graph with more than one reachable terminal.
		fm.AddEdge(self, cell, nil)
		fm.AddEdge(cell, assembleInstance, nil)
	}
	out.Mutated = true
	out.Appended = int32(len(out.Queries) - 1)
	return out, nil
}

// appendError joins attribution clauses with an explicit separator so
// concatenated conditions ("TRUNCATED: ...; NO_GRAPH: ...") stay parseable.
// This field is THIS NODE's full record; it has no route to the caller.
func appendError(out *gen.FanOutPlan, clause string) {
	if out.Error == "" {
		out.Error = clause
		return
	}
	out.Error = out.Error + "; " + clause
}

// appendPlanError records a clause on BOTH the node's own error and the
// caller-visible plan_error channel. The test for which channel a clause
// belongs on is NOT whether the condition is reachable in the flow this node
// was written for — these nodes are published, so they run in graphs their
// author never saw. It is: can this condition leave a caller holding a
// MISLEADING plan? If so it must travel out.
//
//   - TRUNCATED and CELL_ROW_NOT_ZERO: yes. Both hand back a plan with fewer
//     steps than the task decomposed to, presented as if complete.
//   - ALREADY_GROWN (fires on every forked child) and NO_GRAPH (direct
//     invoke): no. The plan the caller gets is correct; the clause is
//     bookkeeping, and surfacing it would put noise in every successful
//     multi-step run.
//   - NO_CELL and NO_JOIN: no, because there is no caller to inform — the
//     echo channel does not exist in those shapes. NO_CELL means there is no
//     SearchStepAt to echo through at all; NO_JOIN means the cell feeds
//     nothing, so no plan is assembled for anyone to be misled by.
func appendPlanError(out *gen.FanOutPlan, clause string) {
	appendError(out, clause)
	if out.PlanError == "" {
		out.PlanError = clause
		return
	}
	out.PlanError = out.PlanError + "; " + clause
}
