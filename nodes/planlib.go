package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// searchBase is the public, unauthenticated lexical search route of the Axiom
// registry (the semantic route requires a caller identity and is tenant-scoped,
// so a marketplace node cannot use it). Overridden in tests.
var searchBase = "https://api.axiomide.com/api/packages/search"

var httpClient = &http.Client{Timeout: 10 * time.Second}

// stopwords are dropped from queries and candidate text before scoring: they
// carry no capability signal and would inflate match fractions.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "for": true,
	"in": true, "on": true, "with": true, "and": true, "or": true, "from": true,
	"by": true, "into": true, "as": true, "at": true, "is": true, "are": true,
	"be": true, "its": true, "it": true, "that": true, "this": true, "then": true,
	"using": true, "use": true, "via": true, "given": true, "get": true,
	// Glue verbs/particles that appear in task phrasing but carry no capability
	// signal ("look up X", "turn Y into Z") — measured live to inflate scores.
	"look": true, "up": true, "lookup": true, "turn": true, "make": true,
	"need": true, "want": true, "should": true, "can": true, "will": true,
	"does": true, "do": true, "has": true, "have": true, "not": true, "no": true,
	"my": true, "your": true, "our": true, "me": true, "you": true, "we": true,
	"one": true, "each": true, "per": true, "all": true, "any": true, "how": true,
	"what": true, "when": true, "where": true, "which": true, "who": true,
	"also": true, "just": true, "so": true, "such": true,
	"more": true, "most": true, "some": true, "other": true, "same": true,
	"about": true, "over": true, "under": true, "out": true, "off": true,
	"if": true, "else": true, "but": true, "than": true, "too": true,
	"them": true, "they": true, "their": true, "there": true, "these": true,
	"those": true, "please": true, "would": true, "could": true,
}

// genericTokens are real words that survive the stopword filter but describe
// data SHAPE or glue rather than a domain ("details", "code", "value"). They
// still count toward a match, but at reduced weight, so a candidate can no
// longer clear the 0.45 threshold on generic-word overlap alone — the defect
// class found live in v0, where "lookup bank details by code" matched a
// pharmaceutical packaging node at 0.67 purely on "details"+"code". Keys are
// the SINGULAR forms (tokenize strips a plural "s" before lookup).
var genericTokens = map[string]bool{
	"detail": true, "code": true, "data": true, "info": true,
	"information": true, "value": true, "item": true, "record": true,
	"entry": true, "list": true, "text": true, "number": true,
	"id": true, "identifier": true, "name": true, "key": true,
	"field": true, "file": true, "format": true, "type": true,
	"result": true, "statu": true /* "status" post-singular */, "content": true,
	"object": true, "element": true, "source": true, "target": true,
	"input": true, "output": true, "level": true, "mode": true,
	"option": true, "part": true, "section": true, "set": true,
	"service": true, "tool": true, "api": true, "web": true,
	"online": true, "user": true, "based": true, "raw": true,
	"plain": true, "simple": true, "common": true, "standard": true,
	"custom": true, "general": true, "specific": true, "single": true,
	"multiple": true, "current": true, "new": true, "full": true,
}

// genericWeight is what a generic token contributes relative to a
// domain-specific token's 1.0. Calibrated so a query like "bank details code"
// scores 0.7/1.7 ≈ 0.41 (< 0.45) when ONLY the generic words match, while a
// two-token query with its one specific word matched scores 1/1.35 ≈ 0.74.
const genericWeight = 0.35

func tokenWeight(t string) float64 {
	if genericTokens[t] {
		return genericWeight
	}
	return 1.0
}

// camelBoundary inserts spaces at lowercase→uppercase boundaries so node names
// like "ValidateIban" tokenize as ["validate","iban"].
func camelBoundary(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// tokenize lowercases, splits on any non-alphanumeric rune (after camel-case
// splitting), and drops stopwords and single-character fragments.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(camelBoundary(s)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, singular(f))
	}
	return out
}

// singular strips a plain plural "s" so "messages" matches "message" — the
// search API stems its matching but this scorer is exact-token, and plural
// drift was losing real matches. Deliberately minimal: no "es"/"ies" logic,
// and "ss" endings ("address") are left alone.
func singular(t string) string {
	if len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
		return t[:len(t)-1]
	}
	return t
}

// apiNode mirrors the fields of the registry's node search response this
// package reads. Extra fields in the response are ignored.
type apiNode struct {
	NodeName    string `json:"node_name"`
	Description string `json:"description"`
	PackageName string `json:"package_name"`
	Version     string `json:"version"`
}

func searchNodes(ctx context.Context, query string) ([]apiNode, error) {
	u := searchBase + "?type=nodes&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, detail)
	}
	var nodes []apiNode
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("decoding search response: %w", err)
	}
	return nodes, nil
}

// scoreCandidate returns the WEIGHTED fraction of the query's informative
// tokens found in the candidate's name + package + description: 1.0 = every
// informative query word matched, 0.0 = none did. Domain-specific tokens
// carry weight 1.0 and generic data-shape tokens genericWeight, so a
// candidate that shares only generic words with the query ("details","code")
// cannot clear the 0.45 match threshold, while missing only a generic word
// barely dents the score.
func scoreCandidate(queryToks []string, n apiNode) float64 {
	if len(queryToks) == 0 {
		return 0
	}
	candToks := make(map[string]bool)
	for _, t := range tokenize(n.NodeName + " " + n.PackageName + " " + n.Description) {
		candToks[t] = true
	}
	var hit, total float64
	for _, qt := range queryToks {
		w := tokenWeight(qt)
		total += w
		if candToks[qt] {
			hit += w
		}
	}
	if total == 0 {
		return 0
	}
	return hit / total
}

// searchOneQuery runs the full-phrase search, relaxes to per-keyword searches
// only when the phrase search legitimately returned nothing, and returns the
// deduped, locally scored, ranked candidates for one step.
func searchOneQuery(ctx context.Context, query string, limit int) *gen.StepCandidates {
	step := &gen.StepCandidates{Query: query}
	queryToks := tokenize(query)

	pool, err := searchNodes(ctx, query)
	if err != nil {
		step.Error = "search: " + err.Error()
		return step
	}
	if len(pool) == 0 && len(queryToks) > 1 {
		// The phrase matched nothing — try each informative keyword alone and
		// pool the results. A keyword search that errors is skipped (the phrase
		// search already proved the API reachable, so a partial relax is safe).
		for _, kw := range queryToks {
			more, kerr := searchNodes(ctx, kw)
			if kerr != nil {
				continue
			}
			pool = append(pool, more...)
		}
		if len(pool) > 0 {
			step.Relaxed = true
		}
	}

	seen := make(map[string]bool)
	nameHits := make(map[*gen.Candidate]int)
	for _, n := range pool {
		key := n.PackageName + "/" + n.NodeName + "@" + n.Version
		if seen[key] {
			continue
		}
		seen[key] = true
		score := scoreCandidate(queryToks, n)
		if score == 0 {
			// No informative query word appears anywhere in the candidate's
			// text — pure pool noise from a relaxed/stemmed API match.
			continue
		}
		c := &gen.Candidate{
			Node:        n.NodeName,
			Package:     n.PackageName,
			Version:     n.Version,
			Description: n.Description,
			Score:       score,
		}
		nameHits[c] = countNameHits(queryToks, n.NodeName)
		step.Candidates = append(step.Candidates, c)
	}
	// Rank by score, then by how many query words appear in the node NAME
	// itself — measured live: "validate iban checksum" ties ValidateBic and
	// ValidateIban at 1.0 on name+package+description text, and only the
	// name-hit count (1 vs 2) separates the right pick from the wrong one.
	sort.SliceStable(step.Candidates, func(i, j int) bool {
		a, b := step.Candidates[i], step.Candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if nameHits[a] != nameHits[b] {
			return nameHits[a] > nameHits[b]
		}
		return a.Node < b.Node
	})
	if len(step.Candidates) > limit {
		step.Candidates = step.Candidates[:limit]
	}
	return step
}

// countNameHits returns how many of the query's informative tokens appear in
// the node's own name — the ranking tiebreaker among equal-score candidates.
func countNameHits(queryToks []string, nodeName string) int {
	nameToks := make(map[string]bool)
	for _, t := range tokenize(nodeName) {
		nameToks[t] = true
	}
	hits := 0
	for _, qt := range queryToks {
		if nameToks[qt] {
			hits++
		}
	}
	return hits
}

// stripCodeFences removes Markdown code fences (``` or ```json) so LLM output
// wrapped in a fenced block parses like bare JSON.
func stripCodeFences(s string) string {
	if !strings.Contains(s, "```") {
		return s
	}
	var kept []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		_ = inFence // content is kept whether fenced or not; only fence markers are dropped
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// queryFromItem extracts a query string from one parsed JSON list element:
// either a bare string or an object under a handful of conventional keys.
func queryFromItem(item any) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"query", "search", "description", "step", "capability", "q"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// queriesFromValue extracts the query list from a parsed JSON value: a bare
// list, or an object wrapping one under "steps" / "queries".
func queriesFromValue(v any) []string {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case map[string]any:
		for _, key := range []string{"steps", "queries"} {
			if list, ok := t[key].([]any); ok {
				items = list
				break
			}
		}
	}
	var out []string
	seen := make(map[string]bool)
	for _, it := range items {
		q := queryFromItem(it)
		if q == "" || seen[strings.ToLower(q)] {
			continue
		}
		seen[strings.ToLower(q)] = true
		out = append(out, q)
	}
	return out
}

// parseQueriesJSON defensively extracts a query list from raw LLM text: fences
// are stripped, then every '[' / '{' position is tried as the start of a JSON
// value until one both parses and yields at least one query.
func parseQueriesJSON(raw string) ([]string, error) {
	s := stripCodeFences(raw)
	for i, r := range s {
		if r != '[' && r != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v any
		if err := dec.Decode(&v); err != nil {
			continue
		}
		if qs := queriesFromValue(v); len(qs) > 0 {
			return qs, nil
		}
	}
	return nil, fmt.Errorf("no JSON query list found in %d bytes of text", len(raw))
}

// runSearches executes one search per query concurrently (bounded), preserving
// input order in the result.
func runSearches(ctx context.Context, queries []string, limit int) []*gen.StepCandidates {
	steps := make([]*gen.StepCandidates, len(queries))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			steps[i] = searchOneQuery(ctx, q, limit)
		}(i, q)
	}
	wg.Wait()
	return steps
}
