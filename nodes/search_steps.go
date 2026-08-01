package nodes

import (
	"context"
	"strings"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// SearchSteps searches the Axiom marketplace catalog for the nodes that cover each of a list of capability queries (one per step of a decomposed task) and returns ranked, scored candidates per step. Ranking is a retrieve-then-rerank stack: the platform's hybrid semantic search retrieves candidates by MEANING (so a step phrased in different words than the node's description still finds it), then one Anthropic call per step reranks them against the step and assigns each a calibrated 0-1 confidence plus a one-line reason for the top pick. Every step reports which stage ranked it in score_basis, because the scales are not comparable and the matched threshold is calibrated per basis. The stack degrades rather than fails: with no ANTHROPIC_API_KEY (a per-node tenant secret — this package holds no key of its own) steps rank semantically, and if the semantic route is unreachable they rank by the legacy lexical word-overlap score; either way the reason is attributed in scoring_error and a plan still comes back. Pin a stage with scoring_mode ("semantic" for no LLM call or cost, "lexical" for the fully deterministic legacy path). Accepts the queries three ways, by precedence: an explicit list (queries), raw LLM text containing a JSON list (queries_json — code fences stripped, common wrapper shapes accepted), or a single query string (query). Steps are scored concurrently, so an N-step plan costs one round-trip of wall time rather than N. A step with zero candidates is a genuine catalog "no match"; a step whose search transport-failed carries a per-step error instead and rules nothing out.
func SearchSteps(ctx context.Context, ax axiom.Context, input *gen.SearchStepsInput) (*gen.SearchStepsResult, error) {
	limit := int(input.Limit)
	if limit <= 0 {
		limit = 5
	}

	var queries []string
	var source string
	switch {
	case len(nonBlank(input.Queries)) > 0:
		queries, source = nonBlank(input.Queries), "queries"
	case strings.TrimSpace(input.QueriesJson) != "":
		parsed, err := parseQueriesJSON(input.QueriesJson)
		if err != nil {
			return &gen.SearchStepsResult{Error: "NO_QUERY: queries_json: " + err.Error()}, nil
		}
		queries, source = parsed, "queries_json"
	case strings.TrimSpace(input.Query) != "":
		queries, source = []string{strings.TrimSpace(input.Query)}, "query"
	default:
		return &gen.SearchStepsResult{Error: "NO_QUERY"}, nil
	}

	steps := scoreSteps(ctx, ax, queries, limit, input.GetScoringMode(), input.GetModel())

	result := &gen.SearchStepsResult{
		Steps:     steps,
		StepCount: int32(len(steps)),
		Source:    source,
	}
	failed := 0
	for _, s := range steps {
		if s.Error != "" {
			failed++
		}
	}
	if failed == len(steps) && len(steps) > 0 {
		result.Error = "ALL_QUERIES_FAILED"
	}
	return result, nil
}

// nonBlank filters a query list to its trimmed, non-empty entries, deduped
// case-insensitively — the same normalization queriesFromValue applies to the
// queries_json path, so both input paths behave identically.
func nonBlank(qs []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, q := range qs {
		t := strings.TrimSpace(q)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}
