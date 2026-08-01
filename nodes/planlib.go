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
	// data-shape / glue nouns
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
	"document": true, "schema": true, "message": true, "payload": true,
	"structure": true, "collection": true, "array": true, "body": true,
	"report": true, "summary": true, "request": true, "response": true,
	// capability verbs — near-universal across the catalog (R15 MAJOR:
	// "parse an XML document and validate its schema" matched a JSON-only
	// node at 0.8 purely on parse+document+validate+schema)
	"parse": true, "validate": true, "convert": true, "extract": true,
	"generate": true, "compute": true, "check": true, "verify": true,
	"resolve": true, "fetch": true, "read": true, "load": true,
	"transform": true, "normalize": true, "render": true, "encode": true,
	"decode": true, "serialize": true, "deserialize": true, "merge": true,
	"split": true, "filter": true, "sort": true, "count": true,
	"compare": true, "process": true, "analyze": true, "search": true,
	"query": true, "update": true, "create": true, "build": true,
	"add": true, "remove": true, "delete": true, "apply": true,
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

// pkgNameSeparators rewrites package-name hyphens/underscores to spaces before
// scoring: "iban-tools" is naming convention, not a domain compound, so the
// compound-context gate below must never read "-tools" as a foreign compound.
var pkgNameSeparators = strings.NewReplacer("-", " ", "_", " ")

// scoreCandidate returns the WEIGHTED fraction of the query's informative
// tokens found in the candidate's name + package + description: 1.0 = every
// informative query word matched, 0.0 = none did. Domain-specific tokens
// carry weight 1.0 and generic data-shape tokens genericWeight, so a
// candidate that shares only generic words with the query ("details","code")
// cannot clear the 0.45 match threshold, while missing only a generic word
// barely dents the score. A domain token that appears in the candidate ONLY
// inside foreign hyphen/underscore compounds ("qr" found solely in "QR-IBAN"
// for a query about QR codes) is coincidental-literal evidence, not a domain
// match: it is counted at genericWeight and does not disarm the halving.
func scoreCandidate(queryToks []string, n apiNode) float64 {
	if len(queryToks) == 0 {
		return 0
	}
	text := n.NodeName + " " + pkgNameSeparators.Replace(n.PackageName) + " " + n.Description
	candToks := make(map[string]bool)
	for _, t := range tokenize(text) {
		candToks[t] = true
	}
	querySet := make(map[string]bool, len(queryToks))
	for _, qt := range queryToks {
		querySet[qt] = true
	}
	runs := tokenRuns(text)
	var hit, total float64
	hasSpecific, specificMatched := false, false
	for _, qt := range queryToks {
		w := tokenWeight(qt)
		total += w
		if w == 1.0 {
			hasSpecific = true
		}
		if candToks[qt] {
			if w == 1.0 && compoundOnly(runs, qt, querySet) {
				hit += genericWeight
				continue
			}
			hit += w
			if w == 1.0 {
				specificMatched = true
			}
		}
	}
	if total == 0 {
		return 0
	}
	score := hit / total
	// Structural guarantee behind the "generic overlap alone cannot carry a
	// match" claim: when the query names at least one domain-specific word
	// and this candidate matched NONE of them, halve the score. A weighted
	// fraction alone still clears 0.45 for verb-heavy queries ("parse an XML
	// document and validate its schema" has four generic words to one
	// specific), because many small weights add up; the halving makes the
	// generic-only ceiling ~0.29 for realistic query lengths.
	if hasSpecific && !specificMatched {
		score *= 0.5
	}
	return score
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

// tokenRun is one maximal alphanumeric run of the candidate's normalized text
// plus the single joining character to its neighbors when they are directly
// hyphen/underscore-attached ("qr-iban" → runs "qr","iban" each recording the
// other as a compound neighbor; "qr iban" records none).
type tokenRun struct {
	tok                 string // singularized lowercase run
	leftJoin, rightJoin string // singularized compound neighbor, "" when not joined by - or _
}

// tokenRuns scans the camel-split, lowercased text into alphanumeric runs and
// records direct hyphen/underscore joins between adjacent runs.
func tokenRuns(text string) []tokenRun {
	runes := []rune(strings.ToLower(camelBoundary(text)))
	alnum := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	type span struct{ start, end int }
	var spans []span
	for i := 0; i < len(runes); {
		if !alnum(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && alnum(runes[j]) {
			j++
		}
		spans = append(spans, span{i, j})
		i = j
	}
	runs := make([]tokenRun, len(spans))
	for k, sp := range spans {
		runs[k].tok = singular(string(runes[sp.start:sp.end]))
		if k > 0 && spans[k-1].end == sp.start-1 && (runes[sp.start-1] == '-' || runes[sp.start-1] == '_') {
			runs[k].leftJoin = runs[k-1].tok
		}
	}
	for k := range runs {
		if k+1 < len(runs) && runs[k+1].leftJoin != "" {
			runs[k].rightJoin = runs[k+1].tok
		}
	}
	return runs
}

// compoundOnly reports whether EVERY occurrence of token in the candidate's
// runs is compound-joined to at least one FOREIGN domain word — a neighbor
// that is informative (≥2 chars, not digits-only, not a stopword), carries
// full domain weight, and is not itself one of the query's tokens — without
// any query token corroborating the compound. One standalone occurrence, one
// occurrence whose joins are merely generic/numeric qualifiers ("qr-code",
// "utf-8"), or one occurrence corroborated by a query token in the same
// compound ("swiss qr-iban" when the query names iban) keeps full credit.
func compoundOnly(runs []tokenRun, token string, querySet map[string]bool) bool {
	foreign := func(nb string) bool {
		return len(nb) >= 2 && !allDigits(nb) && !stopwords[nb] &&
			tokenWeight(nb) == 1.0 && !querySet[nb]
	}
	found := false
	for _, r := range runs {
		if r.tok != token {
			continue
		}
		found = true
		hasForeign := (r.leftJoin != "" && foreign(r.leftJoin)) || (r.rightJoin != "" && foreign(r.rightJoin))
		corroborated := (r.leftJoin != "" && querySet[r.leftJoin]) || (r.rightJoin != "" && querySet[r.rightJoin])
		if !hasForeign || corroborated {
			return false
		}
	}
	return found
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
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
