package nodes_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

// fakeSearch serves the registry's node-search response shape from a canned
// query→results map; unknown queries return an empty list (the API's real
// no-match behavior).
func fakeSearch(t *testing.T, byQuery map[string][]map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		results, ok := byQuery[q]
		if !ok {
			results = []map[string]string{}
		}
		_ = json.NewEncoder(w).Encode(results)
	}))
	t.Cleanup(srv.Close)
	nodes.SetSearchBaseForTest(srv.URL)
	t.Cleanup(func() { nodes.SetSearchBaseForTest("https://api.axiomide.com/api/packages/search") })
	return srv
}

func apiRow(node, pkg, ver, desc string) map[string]string {
	return map[string]string{"node_name": node, "package_name": pkg, "version": ver, "description": desc}
}

func TestSearchSteps_PhraseHitScoresAndRanks(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"validate IBAN checksum": {
			apiRow("GetCountrySpec", "h/iban-tools", "0.1.2", "Return the length spec for a country"),
			apiRow("ValidateIban", "h/iban-tools", "0.1.2", "Validate an IBAN's checksum and structure"),
		},
	})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"validate IBAN checksum"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Error != "" || got.Source != "queries" || got.StepCount != 1 {
		t.Fatalf("bad envelope: %+v", got)
	}
	cands := got.Steps[0].Candidates
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(cands))
	}
	// "validate iban checksum" — ValidateIban's name+description carries all three
	// informative words; GetCountrySpec only shares "iban" via the package name.
	if cands[0].Node != "ValidateIban" || cands[0].Score != 1.0 {
		t.Fatalf("best candidate should be ValidateIban at 1.0, got %s at %v", cands[0].Node, cands[0].Score)
	}
	if cands[1].Score >= cands[0].Score {
		t.Errorf("ranking broken: %v then %v", cands[0].Score, cands[1].Score)
	}
	if got.Steps[0].Relaxed {
		t.Error("phrase hit must not be marked relaxed")
	}
}

func TestSearchSteps_RelaxesOnEmptyPhraseOnly(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		// full phrase misses; individual keywords hit
		"parse": {apiRow("ParseThing", "h/thing-tools", "1.0.0", "Parse a thing")},
		"vcard": {apiRow("ParseVcard", "h/vcard-tools", "1.0.0", "Parse a vCard contact")},
	})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"parse vcard"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.Steps[0]
	if !step.Relaxed {
		t.Fatal("empty phrase + keyword hits must be marked relaxed")
	}
	if len(step.Candidates) != 2 {
		t.Fatalf("want pooled keyword candidates, got %d", len(step.Candidates))
	}
	if step.Candidates[0].Node != "ParseVcard" {
		t.Errorf("ParseVcard matches both words and must rank first, got %s", step.Candidates[0].Node)
	}
}

func TestSearchSteps_EmptyEverywhereIsCleanNoMatch(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"quantum yodel basket"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := got.Steps[0]
	if step.Error != "" || len(step.Candidates) != 0 || step.Relaxed {
		t.Fatalf("a true no-match is empty candidates with no error, got %+v", step)
	}
	if got.Error != "" {
		t.Errorf("no-match is not a failure: %q", got.Error)
	}
}

func TestSearchSteps_QueriesJsonFencedAndWrapped(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"validate email address": {apiRow("ValidateEmail", "h/email-tools", "1", "Validate an email address")},
		"resolve mx records":     {apiRow("ResolveMx", "h/dns-tools", "1", "Resolve MX records for a domain")},
	})
	llmText := "Here is the plan:\n```json\n{\"steps\": [{\"query\": \"validate email address\"}, {\"query\": \"resolve mx records\"}]}\n```\nDone."
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		QueriesJson: llmText,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != "queries_json" || got.StepCount != 2 {
		t.Fatalf("bad envelope: %+v", got)
	}
	if got.Steps[0].Query != "validate email address" || got.Steps[1].Query != "resolve mx records" {
		t.Errorf("step order must follow the JSON, got %q / %q", got.Steps[0].Query, got.Steps[1].Query)
	}
	if len(got.Steps[0].Candidates) == 0 || len(got.Steps[1].Candidates) == 0 {
		t.Error("both steps should have found their candidate")
	}
}

func TestSearchSteps_SingleQueryInput(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"whole task description": {apiRow("N", "h/p", "1", "whole task description handler")},
	})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		Query: "  whole task description  ",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != "query" || got.StepCount != 1 || got.Steps[0].Query != "whole task description" {
		t.Fatalf("bad envelope: %+v", got)
	}
}

func TestSearchSteps_NoQueryAndBadJsonAreCleanErrors(t *testing.T) {
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{})
	if err != nil || got.Error != "NO_QUERY" {
		t.Fatalf("empty input: want NO_QUERY, got %+v / %v", got, err)
	}
	got, err = nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		QueriesJson: "I could not produce a plan, sorry.",
	})
	if err != nil || !strings.HasPrefix(got.Error, "NO_QUERY: queries_json:") {
		t.Fatalf("unparseable json: want NO_QUERY: queries_json prefix, got %+v / %v", got, err)
	}
}

func TestSearchSteps_TransportErrorPerStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream broke", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	nodes.SetSearchBaseForTest(srv.URL)
	t.Cleanup(func() { nodes.SetSearchBaseForTest("https://api.axiomide.com/api/packages/search") })

	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"anything"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got.Steps[0].Error, "HTTP 502") {
		t.Fatalf("step must carry the transport error, got %+v", got.Steps[0])
	}
	if got.Error != "ALL_QUERIES_FAILED" {
		t.Errorf("all steps failing must set ALL_QUERIES_FAILED, got %q", got.Error)
	}
}

func TestParseQueriesJSON_Variants(t *testing.T) {
	cases := map[string][]string{
		`["a b", "c d"]`:     {"a b", "c d"},
		`{"queries": ["x"]}`: {"x"},
		`[{"description": "step one"}, {"q": "two"}]`:   {"step one", "two"},
		"```\n[\"fenced\"]\n```":                        {"fenced"},
		`noise before [{"query":"embedded"}] and after`: {"embedded"},
	}
	for in, want := range cases {
		got, err := nodes.ParseQueriesJSON(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q: got %v want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: got %v want %v", in, got, want)
			}
		}
	}
	if _, err := nodes.ParseQueriesJSON("no json here at all"); err == nil {
		t.Error("garbage must error")
	}
	if _, err := nodes.ParseQueriesJSON(`{"unrelated": true}`); err == nil {
		t.Error("JSON without a query list must error")
	}
}

func TestSearchSteps_QueriesListDedupesLikeQueriesJson(t *testing.T) {
	fakeSearch(t, map[string][]map[string]string{
		"validate iban": {apiRow("ValidateIban", "h/iban-tools", "1", "Validate an IBAN")},
	})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"validate iban", "  validate iban  ", "Validate IBAN", "   ", ""},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StepCount != 1 {
		t.Fatalf("duplicates and blanks must collapse to one step (parity with queries_json), got %d", got.StepCount)
	}
	if got.Steps[0].Query != "validate iban" {
		t.Errorf("first spelling wins, got %q", got.Steps[0].Query)
	}
}

func TestSearchSteps_NegativeLimitFallsToDefault(t *testing.T) {
	rows := make([]map[string]string, 0, 8)
	for _, n := range []string{"A1", "B2", "C3", "D4", "E5", "F6", "G7", "H8"} {
		rows = append(rows, apiRow("Convert"+n, "h/p", "1", "convert units "+n))
	}
	fakeSearch(t, map[string][]map[string]string{"convert units": rows})
	got, err := nodes.SearchSteps(context.Background(), newTestContext(t), &gen.SearchStepsInput{
		ScoringMode: "lexical",
		Queries:     []string{"convert units"},
		Limit:       -5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Steps[0].Candidates) != 5 {
		t.Fatalf("negative limit must fall to the default of 5, got %d", len(got.Steps[0].Candidates))
	}
}
