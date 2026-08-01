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

const producerProtoE2E = `
message POut {
  string download_url = 1;
  string body = 2;
}
message PIn { string q = 1; }
`

const urlConsumerProtoE2E = `
message CIn { string target_url = 1; string label = 2; }
message COut { string result = 1; }
`

const messageOnlyConsumerProtoE2E = `
message Inner { string x = 1; }
message CIn { Inner payload = 1; }
message COut { string result = 1; }
`

func pkgDetailJSON(t *testing.T, nodeName, inMsg, outMsg, proto string) []byte {
	t.Helper()
	src, _ := json.Marshal(proto)
	return []byte(`{"nodes":[{"name":"` + nodeName + `","input":"` + inMsg + `","output":"` + outMsg + `"}],` +
		`"messages":[{"name":"M","proto_source":` + string(src) + `}]}`)
}

func matchedStep(desc, pkg, node, version string) *gen.PlanStep {
	return &gen.PlanStep{Description: desc, Matched: true, Package: pkg, Node: node, Version: version, Score: 0.9}
}

func TestCheckBridges_BridgedEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "fetch-tools"):
			w.Write(pkgDetailJSON(t, "Fetch", "PIn", "POut", producerProtoE2E))
		case strings.Contains(r.URL.Path, "render-tools"):
			w.Write(pkgDetailJSON(t, "Render", "CIn", "COut", urlConsumerProtoE2E))
		default:
			http.Error(w, "package not found", 404)
		}
	}))
	defer srv.Close()
	nodes.SetPlanBaseForTest(srv.URL)

	in := &gen.CheckBridgesInput{
		Feasible: true, PlanBasis: "decomposed", MatchedCount: 2, StepCount: 2, Coverage: 1,
		Steps: []*gen.PlanStep{
			matchedStep("fetch the page", "h/fetch-tools", "Fetch", "1.0.0"),
			matchedStep("render it", "h/render-tools", "Render", "1.0.0"),
		},
	}
	out, err := nodes.CheckBridges(context.Background(), newTestContext(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Feasible || out.BridgeStatus != "bridged" || len(out.Bridges) != 1 {
		t.Fatalf("expected feasible bridged plan, got feasible=%v status=%q bridges=%d",
			out.Feasible, out.BridgeStatus, len(out.Bridges))
	}
	b := out.Bridges[0]
	if b.Verdict != "bridged" || !b.Compatible || b.Via != "url: download_url -> target_url" {
		t.Errorf("bad bridge verdict: %+v", b)
	}
	if b.FromNode != "h/fetch-tools/Fetch@1.0.0" || b.ToNode != "h/render-tools/Render@1.0.0" {
		t.Errorf("bad refs: %+v", b)
	}
	if out.Error != "" {
		t.Errorf("clean plan must keep empty error, got %q", out.Error)
	}
	// The plan itself passes through untouched.
	if out.MatchedCount != 2 || out.StepCount != 2 || out.PlanBasis != "decomposed" || len(out.Steps) != 2 {
		t.Errorf("plan fields not passed through: %+v", out)
	}
}

func TestCheckBridges_BlockedForcesInfeasible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fetch-tools") {
			w.Write(pkgDetailJSON(t, "Fetch", "PIn", "POut", producerProtoE2E))
			return
		}
		w.Write(pkgDetailJSON(t, "Sink", "CIn", "COut", messageOnlyConsumerProtoE2E))
	}))
	defer srv.Close()
	nodes.SetPlanBaseForTest(srv.URL)

	in := &gen.CheckBridgesInput{
		Feasible: true, PlanBasis: "decomposed", MatchedCount: 2, StepCount: 2, Coverage: 1,
		Steps: []*gen.PlanStep{
			matchedStep("fetch the page", "h/fetch-tools", "Fetch", "1.0.0"),
			matchedStep("sink it", "h/sink-tools", "Sink", "1.0.0"),
		},
	}
	out, err := nodes.CheckBridges(context.Background(), newTestContext(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Feasible {
		t.Error("a blocked pair must force feasible=false")
	}
	if out.BridgeStatus != "blocked" || !strings.Contains(out.Error, "bridge: ") {
		t.Errorf("blocked pair must be attributed, got status %q error %q", out.BridgeStatus, out.Error)
	}
	if len(out.Bridges) != 1 || out.Bridges[0].Verdict != "blocked" || out.Bridges[0].Compatible {
		t.Errorf("bad bridge entry: %+v", out.Bridges)
	}
}

func TestCheckBridges_FetchFailureIsUnknownNotInfeasible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "package not found", 404)
	}))
	defer srv.Close()
	nodes.SetPlanBaseForTest(srv.URL)

	in := &gen.CheckBridgesInput{
		Feasible: true, PlanBasis: "decomposed", MatchedCount: 2, StepCount: 2, Coverage: 1,
		Error: "search: step two: HTTP 500", // pre-existing attribution must be kept, bridge errors appended
		Steps: []*gen.PlanStep{
			matchedStep("a", "h/a-tools", "A", "1.0.0"),
			matchedStep("b", "h/b-tools", "B", "1.0.0"),
		},
	}
	out, err := nodes.CheckBridges(context.Background(), newTestContext(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Feasible {
		t.Error("an unverifiable bridge must NOT flip feasible — nothing was proven either way")
	}
	if out.BridgeStatus != "unknown" {
		t.Errorf("expected unknown status, got %q", out.BridgeStatus)
	}
	if !strings.HasPrefix(out.Error, "search: step two: HTTP 500; ") || !strings.Contains(out.Error, "bridge: ") {
		t.Errorf("error attribution wrong: %q", out.Error)
	}
	if out.Bridges[0].Verdict != "unknown" || !strings.Contains(out.Bridges[0].Detail, "fetch failed") {
		t.Errorf("bad unknown entry: %+v", out.Bridges[0])
	}
}

func TestCheckBridges_NoPairsAndPassThrough(t *testing.T) {
	nodes.SetPlanBaseForTest("http://127.0.0.1:1") // must never be contacted

	// Single matched step: nothing to check, everything passes through.
	single := &gen.CheckBridgesInput{
		Feasible: true, PlanBasis: "fallback", MatchedCount: 1, StepCount: 1, Coverage: 1,
		Error: "decompose: no key",
		Steps: []*gen.PlanStep{matchedStep("only", "h/x", "X", "1.0.0")},
		Gaps:  []*gen.Gap{{Description: "g"}},
	}
	out, err := nodes.CheckBridges(context.Background(), newTestContext(t), single)
	if err != nil {
		t.Fatal(err)
	}
	if out.BridgeStatus != "no_pairs" || !out.Feasible || out.PlanBasis != "fallback" ||
		out.Error != "decompose: no key" || len(out.Gaps) != 1 || out.MatchedCount != 1 {
		t.Errorf("pass-through broken: %+v", out)
	}

	// A gap BETWEEN two matched steps breaks adjacency: no pair is checked —
	// the missing middle node's types are unknown, so a verdict would be fiction.
	gapBetween := &gen.CheckBridgesInput{
		Feasible: false, PlanBasis: "decomposed", MatchedCount: 2, StepCount: 3, Coverage: 2.0 / 3,
		Steps: []*gen.PlanStep{
			matchedStep("a", "h/a", "A", "1"),
			{Description: "missing capability"},
			matchedStep("c", "h/c", "C", "1"),
		},
	}
	out, err = nodes.CheckBridges(context.Background(), newTestContext(t), gapBetween)
	if err != nil {
		t.Fatal(err)
	}
	if out.BridgeStatus != "no_pairs" || len(out.Bridges) != 0 {
		t.Errorf("gap-separated picks must not be paired: %+v", out)
	}

	// Empty input restores the plan_basis sentinel.
	out, err = nodes.CheckBridges(context.Background(), newTestContext(t), &gen.CheckBridgesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.PlanBasis != "none" || out.BridgeStatus != "no_pairs" {
		t.Errorf("empty input must yield none/no_pairs, got basis=%q status=%q", out.PlanBasis, out.BridgeStatus)
	}
}

// ---------------------------------------------------------------------------
// Weighted scoring — the canonical v0 false positive ("lookup bank details by
// code" matching a pharma packaging node at 0.667) must now fall below the
// 0.45 threshold, while genuine picks keep their scores.
// ---------------------------------------------------------------------------

func TestScoreCandidate_GenericWordsCannotCarryAMatch(t *testing.T) {
	// Live-captured candidate that v0 scored 0.667 for this query (R14 MAJOR).
	dailymed := nodes.APINode{
		NodeName:    "GetPackagingBySetId",
		PackageName: "christiangeorgelucas/dailymed-connector",
		Version:     "0.1.0",
		Description: "Packaging and product details for a specific DailyMed label, looked up by its setid (from SearchByDrugName): product name(s), product code(s), and primary active-ingredient name(s).",
	}
	q := nodes.Tokenize("lookup bank details by code")
	if s := nodes.ScoreCandidate(q, dailymed); s >= 0.45 {
		t.Errorf("generic-word overlap must stay below the 0.45 threshold, got %.3f", s)
	}

	// A candidate matching the DOMAIN word (bank) but neither generic word
	// must still comfortably clear the threshold.
	bank := nodes.APINode{
		NodeName:    "LookupBank",
		PackageName: "h/bank-registry-tools",
		Version:     "1.0.0",
		Description: "Finds the issuing bank for an account identifier.",
	}
	if s := nodes.ScoreCandidate(q, bank); s < 0.45 {
		t.Errorf("domain-word match must clear the threshold, got %.3f", s)
	}

	// All-specific queries are unaffected: a full match is still exactly 1.0.
	iban := nodes.APINode{
		NodeName:    "ValidateIban",
		PackageName: "christiangeorgelucas/iban-tools",
		Version:     "0.2.0",
		Description: "Validates an IBAN's checksum and structure.",
	}
	if s := nodes.ScoreCandidate(nodes.Tokenize("validate iban checksum"), iban); s != 1.0 {
		t.Errorf("full match must stay 1.0, got %.3f", s)
	}
}

// R15 review MAJOR: capability verbs and shape nouns are near-universal in
// the catalog, so a query like "parse an XML document and validate its
// schema" matched a JSON-only node at 0.8 on parse+document+validate+schema
// overlap while "xml" — the only domain word — went unmatched. The verb/noun
// weights plus the no-domain-word-matched halving must keep such candidates
// below the 0.45 threshold, while a candidate that DOES name the domain word
// still clears it.
func TestScoreCandidate_VerbOverlapCannotCarryAMatch(t *testing.T) {
	q := nodes.Tokenize("parse an XML document and validate its schema")

	// Live-captured candidate that scored 0.8 for this query pre-fix.
	openrpc := nodes.APINode{
		NodeName:    "ParseDocument",
		PackageName: "christiangeorgelucas/openrpc-tools",
		Version:     "0.1.1",
		Description: `Lightweight structural parse of an OpenRPC document: confirms the JSON text parses and has the minimum OpenRPC shape (an "openrpc" version string, an "info" object, and a "methods" array), and reports top-level counts (methods, components.schemas, servers). Does not check every method or schema against the full OpenRPC meta-schema — use ValidateDocument for that. Malformed or oversized input returns valid=false with a structured error instead of throwing.`,
	}
	if s := nodes.ScoreCandidate(q, openrpc); s >= 0.45 {
		t.Errorf("verb/shape-noun overlap without the domain word must stay below threshold, got %.3f", s)
	}

	// A candidate that names the domain word clears the threshold even
	// without matching every verb.
	xmlNode := nodes.APINode{
		NodeName:    "ValidateXml",
		PackageName: "h/xml-tools",
		Version:     "1.0.0",
		Description: "Validates an XML document's well-formedness and checks it against a schema.",
	}
	if s := nodes.ScoreCandidate(q, xmlNode); s < 0.45 {
		t.Errorf("domain-word candidate must clear threshold, got %.3f", s)
	}

	// R15 repro 3: "convert a temperature value" — a candidate matching only
	// convert+value must be sub-threshold; one naming temperature clears.
	q2 := nodes.Tokenize("convert a temperature value")
	ion := nodes.APINode{
		NodeName:    "ConvertText",
		PackageName: "h/ion-tools",
		Version:     "1.0.0",
		Description: "Converts an Ion value between binary and text encodings.",
	}
	if s := nodes.ScoreCandidate(q2, ion); s >= 0.45 {
		t.Errorf("convert+value overlap must stay below threshold, got %.3f", s)
	}
	climate := nodes.APINode{
		NodeName:    "ConvertTemperature",
		PackageName: "h/units-tools",
		Version:     "1.0.0",
		Description: "Converts a temperature value between celsius, fahrenheit, and kelvin.",
	}
	if s := nodes.ScoreCandidate(q2, climate); s < 0.45 {
		t.Errorf("temperature candidate must clear threshold, got %.3f", s)
	}
}
