package nodes

import (
	"context"
	"strings"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// SearchSteps searches the public Axiom marketplace catalog for candidate nodes covering each of a list of capability queries (one query per step of a decomposed task), and returns ranked candidates per step with a locally computed lexical relevance score in [0,1]. Accepts the queries three ways, by precedence: an explicit list (queries), raw LLM text containing a JSON list (queries_json — code fences stripped, common wrapper shapes accepted), or a single query string (query). A step with zero candidates after phrase and relaxed per-keyword search is a genuine catalog "no match"; a step whose search transport-failed carries a per-step error instead and rules nothing out. No authentication and no secrets — it only reads the public catalog.
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

	steps := runSearches(ctx, queries, limit)

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
