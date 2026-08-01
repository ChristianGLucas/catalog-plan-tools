package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// semanticBase is the platform's PUBLIC hybrid-search route (SEARCH-QUALITY
// 2026-07-27): a pgvector cosine leg and a lexical leg, RRF-fused. It needs no
// authentication. Unlike the lexical GET route it returns a thresholdable
// per-result cosine `similarity`, which is what lets a semantic step have a
// calibrated matched/unmatched verdict at all.
var semanticBase = "https://api.axiomide.com/app/marketplace/search/semantic"

// semanticOverfetch is how many candidates retrieval asks for before the judge
// reranks and the caller's limit truncates. Retrieval is cheap and recall is
// the thing it is good at, so it deliberately fetches more than the caller
// wants: the judge can only promote a node retrieval actually surfaced.
const semanticOverfetch = 12

// semanticRow is one row of the hybrid-search response.
type semanticRow struct {
	NodeName    string  `json:"node_name"`
	Description string  `json:"description"`
	PackageName string  `json:"package_name"`
	Version     string  `json:"version"`
	Similarity  float64 `json:"similarity"`
	Score       float64 `json:"score"`
}

// searchSemantic asks the hybrid route for one query's candidates, ranked by
// the route's own fusion, each carrying its cosine similarity as the score.
// An error here is never fatal to a plan: the caller falls back to lexical.
func searchSemantic(ctx context.Context, query string, limit int) ([]*gen.Candidate, error) {
	body, err := json.Marshal(map[string]any{"q": query, "type": "nodes", "limit": limit})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, semanticBase, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// 503 is the documented shape when the server has no embedding key
		// configured — the whole reason the lexical fallback exists.
		return nil, fmt.Errorf("semantic search: HTTP %d", resp.StatusCode)
	}

	var rows []semanticRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("semantic search: decode: %w", err)
	}

	// The route orders rows by its RRF fusion `score`, which blends the vector
	// and lexical legs' RANKS — a useful retrieval order, but not the number
	// this package exposes. `score` here is the cosine `similarity`, because
	// that is the one that is thresholdable, so the list is re-sorted by it:
	// otherwise candidates[0] would not be the highest-scoring candidate and
	// the list would contradict the verdict assemble derives from it.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Similarity > rows[j].Similarity })

	out := make([]*gen.Candidate, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.NodeName) == "" {
			continue
		}
		out = append(out, &gen.Candidate{
			Node:               r.NodeName,
			Package:            r.PackageName,
			Version:            r.Version,
			Description:        r.Description,
			Score:              r.Similarity,
			SemanticSimilarity: r.Similarity,
			ScoreBasis:         basisSemantic,
		})
	}
	return out, nil
}

// retrieve produces one step's candidate pool and says which basis ranked it.
// Semantic retrieval first — it is the stage that fixes RECALL, finding nodes
// whose description means the same thing as the query without sharing its
// words — and the lexical path is the fallback so the planner can never get
// WORSE than it was before this stage existed.
func retrieve(ctx context.Context, query string, limit int, lexicalOnly bool) *gen.StepCandidates {
	if lexicalOnly {
		sc := searchOneQuery(ctx, query, limit)
		sc.ScoreBasis = basisLexical
		markBasis(sc)
		return sc
	}

	cands, err := searchSemantic(ctx, query, semanticOverfetch)
	if err != nil {
		sc := searchOneQuery(ctx, query, limit)
		sc.ScoreBasis = basisLexical
		sc.ScoringError = "semantic: " + err.Error() + "; ranking lexically"
		markBasis(sc)
		return sc
	}
	return &gen.StepCandidates{
		Query:      query,
		Candidates: cands,
		ScoreBasis: basisSemantic,
	}
}

// markBasis stamps every candidate with the step's basis, so a Candidate read
// on its own is still interpretable.
func markBasis(sc *gen.StepCandidates) {
	for _, c := range sc.GetCandidates() {
		c.ScoreBasis = sc.GetScoreBasis()
	}
}

// truncate caps a step's candidate list after ranking, so the cap never
// changes which node is best — only how many runner-ups are reported.
func truncate(sc *gen.StepCandidates, limit int) *gen.StepCandidates {
	if limit > 0 && len(sc.GetCandidates()) > limit {
		sc.Candidates = sc.Candidates[:limit]
	}
	return sc
}
