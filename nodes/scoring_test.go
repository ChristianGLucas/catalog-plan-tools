package nodes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

// fakeSemantic serves the hybrid-search route's response shape from a canned
// query→rows map. Rows carry a cosine similarity, which is the whole reason
// the semantic basis can have a calibrated threshold at all.
func fakeSemantic(t *testing.T, byQuery map[string][]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Q string `json:"q"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rows, ok := byQuery[body.Q]
		if !ok {
			rows = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	nodes.SetSemanticBaseForTest(srv.URL)
	t.Cleanup(func() { nodes.SetSemanticBaseForTest("https://api.axiomide.com/app/marketplace/search/semantic") })
	return srv
}

func semRow(node, pkg, ver, desc string, sim float64) map[string]any {
	return map[string]any{"node_name": node, "package_name": pkg, "version": ver, "description": desc, "similarity": sim}
}

// fakeJudge serves the Anthropic Messages shape, returning whatever ranking
// JSON the test hands it (or a provider error).
func fakeJudge(t *testing.T, text string, providerErr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("the judge must send the tenant's key")
		}
		if providerErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"type": "not_found_error", "message": providerErr},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
		})
	}))
	t.Cleanup(srv.Close)
	nodes.SetAnthropicBaseForTest(srv.URL)
	t.Cleanup(func() { nodes.SetAnthropicBaseForTest("https://api.anthropic.com/v1/messages") })
	return srv
}

// keyedContext is a testContext that resolves ANTHROPIC_API_KEY, so the judge
// stage is reachable.
func keyedContext(t *testing.T) *testContext {
	c := newTestContext(t)
	c.secretsMap["ANTHROPIC_API_KEY"] = "sk-test"
	return c
}

const (
	basisJudgeName    = "judge"
	basisSemanticName = "semantic"
	basisLexicalName  = "lexical"
)

const xmlLinksQuery = "extract links from XML elements"

// semanticPool is the golden set's headline case as retrieval actually returns
// it: xpath-tools IS surfaced (which lexical never managed), but ExtractText
// outranks the correct ExtractAttribute. This is the gap the judge must close.
func semanticPool() map[string][]map[string]any {
	return map[string][]map[string]any{
		xmlLinksQuery: {
			semRow("Extract", "christiangeorgelucas/tavily-connector", "0.1.0", "Extract the main content of a single URL as markdown.", 0.462),
			semRow("ExtractText", "christiangeorgelucas/xpath-tools", "0.1.1", "Extract the text content of every node an XPath expression matches.", 0.472),
			semRow("ExtractAttribute", "christiangeorgelucas/xpath-tools", "0.1.1", "Extract an attribute's value from every element an XPath expression matches — href, xlink:href, src.", 0.455),
		},
	}
}

// Stage 1: retrieval ranks by cosine similarity and stamps the basis, so a
// candidate read on its own is still interpretable.
func TestScoring_SemanticRetrievalRanksAndStampsBasis(t *testing.T) {
	fakeSemantic(t, semanticPool())
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery}, ScoringMode: "semantic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.GetSteps()[0]
	if step.GetScoreBasis() != "semantic" {
		t.Fatalf("basis must be semantic, got %q", step.GetScoreBasis())
	}
	top := step.GetCandidates()[0]
	if top.GetNode() != "ExtractText" {
		t.Fatalf("retrieval ranks by the route's similarity: want ExtractText first, got %q", top.GetNode())
	}
	if top.GetSemanticSimilarity() != 0.472 || top.GetScore() != 0.472 {
		t.Fatalf("the cosine similarity must be both the score and kept verbatim: %+v", top)
	}
	if top.GetScoreBasis() != "semantic" {
		t.Fatalf("every candidate must carry its basis: %+v", top)
	}
}

// Stage 2, and THE point of the upgrade: the judge promotes the node that
// actually does the step over the one that merely retrieves better, and says
// why. This is the shipped failure case (xml-links) end to end.
func TestScoring_JudgeClosesTheGapRetrievalLeaves(t *testing.T) {
	fakeSemantic(t, semanticPool())
	fakeJudge(t, `{"ranking":[
	  {"index":2,"confidence":0.94,"reason":"A link in XML lives in an attribute (href/xlink:href), which is exactly what this extracts."},
	  {"index":1,"confidence":0.42},
	  {"index":0,"confidence":0.11}]}`, "")

	got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.GetSteps()[0]
	if step.GetScoreBasis() != "judge" {
		t.Fatalf("basis must be judge, got %q (scoring_error %q)", step.GetScoreBasis(), step.GetScoringError())
	}
	top := step.GetCandidates()[0]
	if top.GetNode() != "ExtractAttribute" {
		t.Fatalf("the judge must promote the node that does the step, got %q", top.GetNode())
	}
	if top.GetScore() != 0.94 {
		t.Fatalf("score must become the judge's calibrated confidence, got %v", top.GetScore())
	}
	if top.GetSemanticSimilarity() != 0.455 {
		t.Fatalf("the retrieval similarity must survive the rerank so both stages stay readable: %+v", top)
	}
	if !strings.Contains(top.GetPickReason(), "attribute") {
		t.Fatalf("the top pick must carry the judge's reason: %q", top.GetPickReason())
	}
	// Only the top pick is justified — a reason on every runner-up would be
	// noise the caller has to filter.
	if step.GetCandidates()[1].GetPickReason() != "" {
		t.Fatalf("only the top pick carries a reason: %+v", step.GetCandidates()[1])
	}
}

// The degradation chain, rung by rung. None of these may fail the step.
func TestScoring_DegradationChainIsAttributedNeverFatal(t *testing.T) {
	t.Run("no key -> semantic", func(t *testing.T) {
		fakeSemantic(t, semanticPool())
		got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
			Queries: []string{xmlLinksQuery},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		step := got.GetSteps()[0]
		if step.GetScoreBasis() != "semantic" || len(step.GetCandidates()) == 0 {
			t.Fatalf("a missing key must degrade to semantic, not fail: %+v", step)
		}
		if !strings.Contains(step.GetScoringError(), "ANTHROPIC_API_KEY") {
			t.Fatalf("the missing key must be attributed: %q", step.GetScoringError())
		}
	})

	t.Run("judge errors -> semantic", func(t *testing.T) {
		fakeSemantic(t, semanticPool())
		fakeJudge(t, "", "model: claude-not-real")
		got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
			Queries: []string{xmlLinksQuery},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		step := got.GetSteps()[0]
		if step.GetScoreBasis() != "semantic" || len(step.GetCandidates()) == 0 {
			t.Fatalf("a judge failure must degrade to semantic, not fail: %+v", step)
		}
		if !strings.Contains(step.GetScoringError(), "claude-not-real") {
			t.Fatalf("the provider's own words must survive: %q", step.GetScoringError())
		}
	})

	t.Run("semantic route down -> lexical", func(t *testing.T) {
		// The route answers 503, which is its documented shape on a server
		// with no embedding key. Not live-forceable, so it is pinned here.
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer down.Close()
		nodes.SetSemanticBaseForTest(down.URL)
		defer nodes.SetSemanticBaseForTest("https://api.axiomide.com/app/marketplace/search/semantic")
		fakeSearch(t, map[string][]map[string]string{
			xmlLinksQuery: {apiRow("ExtractText", "christiangeorgelucas/xpath-tools", "0.1.1", "Extract the text content of XML elements")},
		})

		got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
			Queries: []string{xmlLinksQuery},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		step := got.GetSteps()[0]
		if step.GetScoreBasis() != "lexical" || len(step.GetCandidates()) == 0 {
			t.Fatalf("an unreachable route must fall back to lexical, not fail: %+v", step)
		}
		if !strings.Contains(step.GetScoringError(), "semantic:") {
			t.Fatalf("the fallback must be attributed: %q", step.GetScoringError())
		}
		if step.GetCandidates()[0].GetScoreBasis() != "lexical" {
			t.Fatalf("fallback candidates must be stamped lexical: %+v", step.GetCandidates()[0])
		}
	})
}

// A pinned mode is a promise: "semantic" must never spend an LLM call even
// when a key is present, and "lexical" must never touch the semantic route.
func TestScoring_PinnedModesSkipTheStagesTheyExclude(t *testing.T) {
	judgeCalls := 0
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": `{"ranking":[{"index":0,"confidence":1}]}`}}})
	}))
	defer judge.Close()
	nodes.SetAnthropicBaseForTest(judge.URL)
	defer nodes.SetAnthropicBaseForTest("https://api.anthropic.com/v1/messages")

	semanticCalls := 0
	sem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		semanticCalls++
		_ = json.NewEncoder(w).Encode([]map[string]any{
			semRow("ExtractText", "christiangeorgelucas/xpath-tools", "0.1.1", "Extract text", 0.47),
		})
	}))
	defer sem.Close()
	nodes.SetSemanticBaseForTest(sem.URL)
	defer nodes.SetSemanticBaseForTest("https://api.axiomide.com/app/marketplace/search/semantic")

	fakeSearch(t, map[string][]map[string]string{
		xmlLinksQuery: {apiRow("ExtractText", "christiangeorgelucas/xpath-tools", "0.1.1", "Extract the text content of XML elements")},
	})

	if _, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery}, ScoringMode: "semantic",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if judgeCalls != 0 {
		t.Fatalf("scoring_mode=semantic must not spend an LLM call, made %d", judgeCalls)
	}

	semanticCalls = 0
	if _, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery}, ScoringMode: "lexical",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if semanticCalls != 0 || judgeCalls != 0 {
		t.Fatalf("scoring_mode=lexical must touch neither stage: semantic=%d judge=%d", semanticCalls, judgeCalls)
	}
}

// Nothing retrieved is a real "no match" — the judge is never asked, because
// asking it would only invite it to invent a node that does not exist.
func TestScoring_EmptyRetrievalNeverReachesTheJudge(t *testing.T) {
	fakeSemantic(t, nil)
	judgeCalls := 0
	judge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { judgeCalls++ }))
	defer judge.Close()
	nodes.SetAnthropicBaseForTest(judge.URL)
	defer nodes.SetAnthropicBaseForTest("https://api.anthropic.com/v1/messages")

	got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{"run monthly payroll and file the tax withholdings"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.GetSteps()[0].GetCandidates()) != 0 {
		t.Fatalf("no retrieval hits must stay a clean no-match")
	}
	if judgeCalls != 0 {
		t.Fatalf("the judge must not be asked to rank an empty pool, called %d times", judgeCalls)
	}
}

// A hallucinated or duplicated index would silently promote the wrong node —
// the parser drops them rather than trusting the model's arithmetic.
func TestScoring_JudgeRankingIsSanitised(t *testing.T) {
	fakeSemantic(t, semanticPool())
	fakeJudge(t, "```json\n"+`{"ranking":[
	  {"index":9,"confidence":0.99,"reason":"hallucinated index"},
	  {"index":2,"confidence":1.7,"reason":"out-of-range confidence"},
	  {"index":2,"confidence":0.2},
	  {"index":0,"confidence":-3}]}`+"\n```", "")

	got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.GetSteps()[0]
	top := step.GetCandidates()[0]
	if top.GetNode() != "ExtractAttribute" {
		t.Fatalf("the out-of-range index must be dropped, not applied: got %q", top.GetNode())
	}
	if top.GetScore() != 1.0 {
		t.Fatalf("confidence must be clamped into [0,1], got %v", top.GetScore())
	}
	if len(step.GetCandidates()) != 3 {
		t.Fatalf("every retrieved candidate must survive the rerank, got %d", len(step.GetCandidates()))
	}
	// The duplicate index must not double-count a candidate into the list.
	seen := map[string]bool{}
	for _, c := range step.GetCandidates() {
		if seen[c.GetNode()] {
			t.Fatalf("candidate %q appears twice after reranking", c.GetNode())
		}
		seen[c.GetNode()] = true
	}
}

// Unparseable model output degrades like any other judge failure: the
// retrieved ranking stands, attributed.
func TestScoring_UnparseableJudgeOutputDegrades(t *testing.T) {
	fakeSemantic(t, semanticPool())
	fakeJudge(t, "Sure! Here are my thoughts on these nodes, in prose.", "")
	got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.GetSteps()[0]
	if step.GetScoreBasis() != "semantic" || step.GetCandidates()[0].GetNode() != "ExtractText" {
		t.Fatalf("unparseable output must leave the retrieved ranking intact: %+v", step)
	}
	if !strings.Contains(step.GetScoringError(), "judge:") {
		t.Fatalf("the parse failure must be attributed: %q", step.GetScoringError())
	}
}

// The per-basis thresholds are the load-bearing calibration of the whole
// stack: they are what makes a "matched" verdict mean the same thing whichever
// stage ranked the step. Pinned here with the live golden-set evidence that
// produced them (testdata/golden.json, run 2026-08-01 against the deployed
// 0.7.0 node), so a future edit has to argue with the data.
func TestScoring_ThresholdsSitInTheMeasuredGaps(t *testing.T) {
	// Observed on the golden set — the highest score a NO-MATCH case reached,
	// and the lowest a correct pick reached. The threshold must separate them.
	for _, tc := range []struct {
		basis           string
		worstNoMatch    float64 // must stay UNMATCHED
		weakestTruePick float64 // must be MATCHED
	}{
		// judge: payroll 0.250 / book-flight 0.180 vs weakest correct 0.720
		{basisJudgeName, 0.250, 0.720},
		// semantic: payroll 0.275 / book-flight 0.222 vs weakest correct 0.438
		{basisSemanticName, 0.275, 0.438},
		// lexical: payroll 0.252 / book-flight 0.200 vs weakest correct 0.574
		{basisLexicalName, 0.252, 0.574},
	} {
		th := nodes.ThresholdFor(tc.basis)
		if tc.worstNoMatch >= th {
			t.Fatalf("%s: threshold %.2f would MATCH a no-real-match case scoring %.3f — the planner would become a confident liar", tc.basis, th, tc.worstNoMatch)
		}
		if tc.weakestTruePick < th {
			t.Fatalf("%s: threshold %.2f would REJECT a correct pick scoring %.3f", tc.basis, th, tc.weakestTruePick)
		}
	}
	// An unknown basis must fall back to the strictest historical default
	// rather than silently matching everything.
	if nodes.ThresholdFor("something-new") != nodes.ThresholdFor(basisLexicalName) {
		t.Fatalf("an unrecognised basis must use the lexical default")
	}
}

// R20 MINOR-1, at exactly the reviewer's probe shape: retrieval returns a
// high-similarity candidate the judge then OMITS from its ranking. Before the
// fix the plan scored that unjudged candidate — labelled score_basis "judge",
// carrying a cosine similarity measured against the judge's 0.55 threshold,
// with the judge's pick_reason dropped, and NOT equal to candidates[0].
func TestScoring_UnjudgedCandidateCannotWinAJudgedStep(t *testing.T) {
	const q = "the step the judge actually read"
	// Run the same shape either side of the judge threshold (0.55): the
	// unjudged HighSim scores 0.52, so pre-fix it won BOTH — silently turning
	// the low case into the wrong gap and the high case into the wrong match.
	for _, tc := range []struct {
		confidence float64
		wantMatch  bool
	}{{0.50, false}, {0.60, true}} {
		fakeSemantic(t, map[string][]map[string]any{
			q: {
				semRow("HighSim", "h/wrong-tools", "1.0.0", "Retrieves well, does something else.", 0.52),
				semRow("Chosen", "h/right-tools", "1.0.0", "Does exactly this step.", 0.30),
			},
		})
		// The judge ranks ONLY index 1 (Chosen) and omits HighSim entirely.
		fakeJudge(t, fmt.Sprintf(`{"ranking":[{"index":1,"confidence":%v,"reason":"this is the one that does the step"}]}`, tc.confidence), "")

		search, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{Queries: []string{q}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		step := search.GetSteps()[0]
		if step.GetScoreBasis() != "judge" {
			t.Fatalf("precondition: the step must be judged, got %q", step.GetScoreBasis())
		}
		// The judged candidate leads the list even though it scores lower on
		// the raw number — ordering is (basis, then score).
		if step.GetCandidates()[0].GetNode() != "Chosen" {
			t.Fatalf("judged candidates must lead the list, got %q", step.GetCandidates()[0].GetNode())
		}

		plan, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
			Primary: search.GetSteps(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ps := plan.GetSteps()[0]
		// THE killing assertion: the step is scored on the judged candidate's
		// confidence, never on the unjudged 0.52 cosine.
		if ps.GetScore() != tc.confidence {
			t.Fatalf("confidence %v: the step must be scored on the JUDGED candidate, got %v", tc.confidence, ps.GetScore())
		}
		if ps.GetScoreBasis() != "judge" {
			t.Fatalf("confidence %v: score and basis must describe the same number, got basis %q", tc.confidence, ps.GetScoreBasis())
		}
		if ps.GetMatched() != tc.wantMatch {
			t.Fatalf("confidence %v: want matched=%v against the judge threshold, got %v", tc.confidence, tc.wantMatch, ps.GetMatched())
		}
		if !tc.wantMatch {
			// An honest gap, decided on a judge confidence rather than a stray cosine.
			if len(plan.GetGaps()) != 1 {
				t.Fatalf("confidence %v: must be a gap, got %d", tc.confidence, len(plan.GetGaps()))
			}
			continue
		}
		if ps.GetPackage() != "h/right-tools" || ps.GetNode() != "Chosen" {
			t.Fatalf("the pick must come from the JUDGED subset, got %q/%q", ps.GetPackage(), ps.GetNode())
		}
		if ps.GetPickReason() == "" {
			t.Fatalf("the judge's reason must survive to the plan")
		}
		// And the pick IS candidates[0] — the consistency retrievelib's
		// re-sort comment exists to protect.
		if ps.GetNode() != step.GetCandidates()[0].GetNode() {
			t.Fatalf("the plan's pick must be candidates[0]")
		}
	}
}

// R20 MINOR-2: the ADR-156 revoked-vs-unset distinction was entirely unguarded
// — the reviewer inverted the branch AND deleted it, and the suite stayed
// green. These two sub-tests pin the exact wording of both branches, because
// they tell a caller to do two different things.
func TestScoring_JudgeUnavailableDistinguishesRevokedFromUnset(t *testing.T) {
	pool := semanticPool()

	t.Run("never configured", func(t *testing.T) {
		fakeSemantic(t, pool)
		got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
			Queries: []string{xmlLinksQuery},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		se := got.GetSteps()[0].GetScoringError()
		if !strings.Contains(se, "no ANTHROPIC_API_KEY configured for this tenant") {
			t.Fatalf("an unset secret must say so (check axiom.yaml), got %q", se)
		}
		if strings.Contains(se, "revoked") {
			t.Fatalf("an unset secret must NOT be reported as revoked: %q", se)
		}
	})

	t.Run("revoked mid-execution", func(t *testing.T) {
		fakeSemantic(t, pool)
		ctx := newTestContext(t)
		// Present in the execution's initial snapshot, gone from the live
		// vault — Get() is ("",false) exactly as for unset, so only Status
		// tells them apart.
		ctx.revokedNames["ANTHROPIC_API_KEY"] = true
		got, err := nodes.SearchSteps(context.Background(), ctx, &gen.SearchStepsInput{
			Queries: []string{xmlLinksQuery},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		se := got.GetSteps()[0].GetScoringError()
		if !strings.Contains(se, "ANTHROPIC_API_KEY was revoked during this execution") {
			t.Fatalf("a revoked secret must say so (re-authorize), got %q", se)
		}
		if strings.Contains(se, "no ANTHROPIC_API_KEY configured") {
			t.Fatalf("a revoked secret must NOT be reported as never-configured: %q", se)
		}
	})
}

// R20 MINOR-3: the model returns a ranking but omits the prose. The field must
// still say something true — the confidence it assigned and that no written
// reason came back — never an invented rationale and never a bare empty that
// reads as "the judge had nothing to say".
func TestScoring_PickReasonFallsBackWhenTheModelOmitsIt(t *testing.T) {
	fakeSemantic(t, semanticPool())
	fakeJudge(t, `{"ranking":[{"index":2,"confidence":0.91},{"index":1,"confidence":0.4},{"index":0,"confidence":0.1}]}`, "")

	got, err := nodes.SearchSteps(context.Background(), keyedContext(t), &gen.SearchStepsInput{
		Queries: []string{xmlLinksQuery},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	top := got.GetSteps()[0].GetCandidates()[0]
	if top.GetNode() != "ExtractAttribute" {
		t.Fatalf("precondition: the judge's pick must lead, got %q", top.GetNode())
	}
	reason := top.GetPickReason()
	if !strings.Contains(reason, "0.91") || !strings.Contains(reason, "no written reason") {
		t.Fatalf("the fallback must state the confidence and that no prose came back, got %q", reason)
	}
	// Runner-ups still carry no reason at all.
	if got.GetSteps()[0].GetCandidates()[1].GetPickReason() != "" {
		t.Fatalf("only the top pick is justified")
	}
}
