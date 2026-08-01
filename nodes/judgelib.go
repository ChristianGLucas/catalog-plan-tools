package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// The three stages of the scoring stack, best first. A step's basis says which
// one actually ranked it, and the matched/unmatched threshold is calibrated per
// basis because the three scales are not comparable (see thresholdFor).
const (
	basisJudge    = "judge"
	basisSemantic = "semantic"
	basisLexical  = "lexical"
)

// anthropicBase is the Messages API. Called directly over HTTPS with the
// tenant's own key — the same bring-your-own-key pattern the connector nodes
// use; this package never holds a key of its own.
var anthropicBase = "https://api.anthropic.com/v1/messages"

// defaultJudgeModel matches the planner flows' decompose default, so one
// `model` knob governs the whole plan.
const defaultJudgeModel = "claude-haiku-4-5"

// judgeClient gets a longer timeout than the search client: one rerank call
// reads a dozen node descriptions.
var judgeClient = &http.Client{Timeout: 45 * time.Second}

// judgePrompt asks for a strict-JSON rerank of the retrieved candidates. It is
// deliberately a RERANK: the model may only reorder and score what retrieval
// found, and is told so — a planner that invented node names would produce
// plans that cannot be built.
func judgePrompt(query string, cands []*gen.Candidate) string {
	var b strings.Builder
	b.WriteString("You are ranking published nodes from a data-processing marketplace by how well each one covers ONE capability step of a build plan.\n\n")
	b.WriteString("Step: " + query + "\n\nCandidates:\n")
	for i, c := range cands {
		desc := c.GetDescription()
		if len(desc) > 700 {
			desc = desc[:700] + "…"
		}
		fmt.Fprintf(&b, "[%d] %s/%s@%s — %s\n", i, c.GetPackage(), c.GetNode(), c.GetVersion(), desc)
	}
	b.WriteString(`
Rank ONLY these candidates; never invent a node. Judge whether the node's DESCRIBED capability actually performs the step, not whether its words look similar: a node from the right domain that does a different job is a poor match, and a node whose name shares no words with the step but does exactly that job is an excellent one.

Give each candidate a calibrated confidence in [0,1] that it covers the step:
  0.9-1.0  does exactly this step
  0.7-0.9  does this step with a caveat (extra inputs, a superset, a near-synonym operation)
  0.4-0.7  related and possibly usable, but not this step
  0.0-0.4  wrong job or wrong domain
If NO candidate really covers the step, say so with low confidences — an honest "nothing covers this" is more useful than a confident wrong pick, because the caller turns it into a "build this" gap.

Respond with ONLY a JSON object of the exact form:
{"ranking":[{"index":0,"confidence":0.93,"reason":"one short sentence, only for the best candidate"}]}
Order the array best first, include every candidate index exactly once, and set "reason" only on the first element.`)
	return b.String()
}

type judgeRank struct {
	Index      int     `json:"index"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// callAnthropic performs the single Messages request and returns the model's
// text content.
func callAnthropic(ctx context.Context, apiKey, model, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicBase, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := judgeClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("HTTP %d: decode: %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		// The provider's own words, not a paraphrase — a caller debugging a
		// degraded plan needs the real reason (bad key, unknown model, 529).
		return "", fmt.Errorf("%s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var text strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", fmt.Errorf("empty response")
	}
	return text.String(), nil
}

// parseJudgeRanking extracts the ranking from the model's text, tolerating the
// code fences and stray prose a model adds despite instructions — the same
// defensive posture parseQueriesJSON takes with the decomposition.
func parseJudgeRanking(raw string, n int) ([]judgeRank, error) {
	s := stripCodeFences(raw)
	for i, r := range s {
		if r != '{' {
			continue
		}
		var parsed struct {
			Ranking []judgeRank `json:"ranking"`
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&parsed); err != nil || len(parsed.Ranking) == 0 {
			continue
		}
		var out []judgeRank
		seen := make(map[int]bool)
		for _, r := range parsed.Ranking {
			// An index the model hallucinated would silently promote the wrong
			// node, so out-of-range and duplicate entries are dropped.
			if r.Index < 0 || r.Index >= n || seen[r.Index] {
				continue
			}
			seen[r.Index] = true
			if r.Confidence < 0 {
				r.Confidence = 0
			}
			if r.Confidence > 1 {
				r.Confidence = 1
			}
			out = append(out, r)
		}
		if len(out) == 0 {
			continue
		}
		return out, nil
	}
	return nil, fmt.Errorf("no usable JSON ranking in the response")
}

// judgeStep reranks one step's retrieved candidates with a single LLM call and
// rewrites their scores as the judge's calibrated confidence. It NEVER fails
// the step: on any problem the step keeps its retrieved ranking and the reason
// is attributed on scoring_error, one stage weaker but still a plan.
func judgeStep(ctx context.Context, sc *gen.StepCandidates, apiKey, model string) *gen.StepCandidates {
	cands := sc.GetCandidates()
	if len(cands) == 0 {
		// Nothing retrieved is a real "no match" — there is nothing to rerank,
		// and asking the model would only invite it to invent one.
		return sc
	}
	if strings.TrimSpace(model) == "" {
		model = defaultJudgeModel
	}

	raw, err := callAnthropic(ctx, apiKey, model, judgePrompt(sc.GetQuery(), cands))
	if err != nil {
		sc.ScoringError = joinClause(sc.GetScoringError(), "judge: "+err.Error()+"; ranking "+basisAdverb(sc.GetScoreBasis()))
		return sc
	}
	ranking, err := parseJudgeRanking(raw, len(cands))
	if err != nil {
		sc.ScoringError = joinClause(sc.GetScoringError(), "judge: "+err.Error()+"; ranking "+basisAdverb(sc.GetScoreBasis()))
		return sc
	}

	// Stable best-first order by the judge's confidence. Candidates the model
	// omitted keep their retrieved order at the end, below every judged one,
	// and keep an honest un-judged score rather than a fabricated zero.
	sort.SliceStable(ranking, func(i, j int) bool { return ranking[i].Confidence > ranking[j].Confidence })
	judged := make([]*gen.Candidate, 0, len(cands))
	used := make(map[int]bool, len(ranking))
	for _, r := range ranking {
		c := cands[r.Index]
		c.Score = r.Confidence
		c.ScoreBasis = basisJudge
		used[r.Index] = true
		judged = append(judged, c)
	}
	for i, c := range cands {
		if !used[i] {
			judged = append(judged, c)
		}
	}
	if len(judged) > 0 {
		// The reason is best-effort: the model is asked for one and usually
		// gives it, but it does omit the field sometimes (observed live). An
		// empty pick_reason on a judged step reads as "the judge had nothing to
		// say", which is not what happened — so the fallback states exactly what
		// IS known, and says the model gave no prose rather than inventing any
		// (R20 MINOR-3).
		judged[0].PickReason = strings.TrimSpace(ranking[0].Reason)
		if judged[0].PickReason == "" {
			judged[0].PickReason = fmt.Sprintf("Judged the best match for this step at %.2f confidence; the model returned no written reason.", ranking[0].Confidence)
		}
	}

	sc.Candidates = judged
	sc.ScoreBasis = basisJudge
	return sc
}

// basisAdverb renders a basis as the adverb the attribution reads with, so a
// degraded step says "ranking semantically", not "ranking semanticly".
func basisAdverb(basis string) string {
	switch basis {
	case basisSemantic:
		return "semantically"
	case basisJudge:
		return "by judge"
	default:
		return "lexically"
	}
}

// joinClause appends an attribution clause with the package's "; " separator.
func joinClause(existing, clause string) string {
	if existing == "" {
		return clause
	}
	return existing + "; " + clause
}
