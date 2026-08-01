package nodes

import (
	"strings"
	"testing"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"gopkg.in/yaml.v3"
)

func step(desc, node, pkg, version string, matched bool) *gen.PlanStep {
	return &gen.PlanStep{Description: desc, Node: node, Package: pkg, Version: version, Matched: matched}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Verify an IBAN & look up the issuing bank": "verify-an-iban-look-up-the-issuing-bank",
		"":       "planned-flow",
		"!!!":    "planned-flow",
		"Ab  Cd": "ab-cd",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := slugify("a very long task description that keeps going and going and going far past the cap"); len(got) > 48 {
		t.Errorf("slug not capped: %q (%d)", got, len(got))
	}
}

// parseVia must invert judgeBridge's actual via format — build a real bridged
// verdict and round-trip it, so a format drift in either place fails here.
func TestParseVia_RoundTripsJudgeBridgeFormat(t *testing.T) {
	prod := mkPorts(t, producerProto, "PIn", "POut", "h/p/A@1")
	cons := mkPorts(t, `
message CIn { string target_url = 1; }
message COut { string r = 1; }
`, "CIn", "COut", "h/p/B@1")
	bc := judgeBridge(prod, cons)
	if bc.Verdict != "bridged" {
		t.Fatalf("fixture must bridge, got %+v", bc)
	}
	carrier, outPath, inField, ok := parseVia(bc.Via)
	if !ok || carrier != "url" || outPath != "download_url" || inField != "target_url" {
		t.Errorf("parseVia(%q) = (%q,%q,%q,%v)", bc.Via, carrier, outPath, inField, ok)
	}
}

const skConsumerProto = `
message CIn {
  // The address to fetch.
  string target_url = 1;
  int32 max_bytes = 2;
  repeated string headers = 3;
}
message COut { string body = 1; }
`

func skPorts(t *testing.T) map[string]*nodePorts {
	t.Helper()
	return map[string]*nodePorts{
		"h/fetch-tools/FetchPage@1.0.0": mkPorts(t, skConsumerProto, "CIn", "COut", "h/fetch-tools/FetchPage@1.0.0"),
		"h/dl-tools/Resolve@1.0.0":      mkPorts(t, producerProto, "PIn", "POut", "h/dl-tools/Resolve@1.0.0"),
	}
}

func TestRenderSkeleton_BridgedPair(t *testing.T) {
	steps := []*gen.PlanStep{
		step("resolve the download", "Resolve", "h/dl-tools", "1.0.0", true),
		step("fetch the page", "FetchPage", "h/fetch-tools", "1.0.0", true),
	}
	ports := skPorts(t)
	// producer first: Resolve's input PIn{q string} drives the facade.
	ports["h/dl-tools/Resolve@1.0.0"] = mkPorts(t, `
message PIn {
  // The release to resolve.
  string q = 1;
}
message POut { string download_url = 1; string body = 2; }
`, "PIn", "POut", "h/dl-tools/Resolve@1.0.0")
	bridges := []*gen.BridgeCheck{{
		FromNode: "h/dl-tools/Resolve@1.0.0", ToNode: "h/fetch-tools/FetchPage@1.0.0",
		Verdict: "bridged", Compatible: true, Via: "url: download_url -> target_url",
	}}
	y := renderSkeleton("Fetch a release page", steps, bridges, ports)

	for _, want := range []string{
		"name: your-handle/fetch-a-release-page",
		"message_name: FetchAReleasePageInput",
		"description: 'The release to resolve.'",
		"- alias: resolve",
		"package: h/dl-tools@1.0.0",
		"- alias: fetch_page",
		"from: '@flow_input'",
		"q: q",
		"target_url: download_url",
		`carrier "url" evidence`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("skeleton missing %q:\n%s", want, y)
		}
	}
	// The header comment legitimately mentions TODO(planner); the body (facade,
	// nodes, edges) of a fully bridged, fully documented skeleton must not.
	if body := y[strings.Index(y, "name:"):]; strings.Contains(body, "TODO(planner)") {
		t.Errorf("fully bridged skeleton must carry no TODO markers:\n%s", y)
	}
	assertYAMLFlow(t, y, 2, 2)
}

func TestRenderSkeleton_PlausibleProposalAndRepeatedFacade(t *testing.T) {
	steps := []*gen.PlanStep{
		step("fetch the page", "FetchPage", "h/fetch-tools", "1.0.0", true),
		step("resolve the download", "Resolve", "h/dl-tools", "1.0.0", true),
	}
	ports := skPorts(t)
	bridges := []*gen.BridgeCheck{{
		FromNode: "h/fetch-tools/FetchPage@1.0.0", ToNode: "h/dl-tools/Resolve@1.0.0",
		Verdict: "plausible", Detail: "no shared typed carrier",
	}}
	y := renderSkeleton("", steps, bridges, ports)

	for _, want := range []string{
		"name: your-handle/planned-flow",
		"TODO(planner): no carrier evidence",
		"q: body", // proposed pairing: shortest string leaf -> first string input
		"repeated: true",
		"max_bytes:",
		"kind: int32",
		"TODO(planner): describe this field", // max_bytes has no proto desc
	} {
		if !strings.Contains(y, want) {
			t.Errorf("skeleton missing %q:\n%s", want, y)
		}
	}
	assertYAMLFlow(t, y, 2, 2)
}

func TestRenderSkeleton_GapAndBlocked(t *testing.T) {
	steps := []*gen.PlanStep{
		step("resolve the download", "Resolve", "h/dl-tools", "1.0.0", true),
		step("transmute lead into gold", "", "", "", false),
		step("fetch the page", "FetchPage", "h/fetch-tools", "1.0.0", true),
	}
	y := renderSkeleton("gap task", steps, nil, skPorts(t))
	for _, want := range []string{
		"NO catalog match",
		"transmute lead into gold",
		"separated by unmatched step",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("skeleton missing %q:\n%s", want, y)
		}
	}
	// The two picks must NOT be wired across the gap, and the unreachable
	// second pick must degrade to a commented stub (a validating flow needs
	// exactly one terminal node — live calibration finding).
	if strings.Contains(y, "\n    - from: resolve\n") {
		t.Errorf("edge proposed across a gap:\n%s", y)
	}
	if !strings.Contains(y, "# - alias: fetch_page") {
		t.Errorf("beyond-the-break pick must be a commented stub:\n%s", y)
	}
	assertYAMLFlow(t, y, 1, 1) // one real node + the @flow_input edge

	blocked := []*gen.BridgeCheck{{
		FromNode: "h/dl-tools/Resolve@1.0.0", ToNode: "h/fetch-tools/FetchPage@1.0.0",
		Verdict: "blocked", Detail: "type sets are disjoint",
	}}
	adjacent := []*gen.PlanStep{steps[0], steps[2]}
	y2 := renderSkeleton("blocked task", adjacent, blocked, skPorts(t))
	if !strings.Contains(y2, "BLOCKED") || !strings.Contains(y2, "type sets are disjoint") {
		t.Errorf("blocked commentary missing:\n%s", y2)
	}
	if !strings.Contains(y2, "# - alias: fetch_page") {
		t.Errorf("blocked consumer must be a commented stub:\n%s", y2)
	}
	assertYAMLFlow(t, y2, 1, 1)
}

func TestRenderSkeleton_NoMatchesAndAliasDedupe(t *testing.T) {
	if y := renderSkeleton("t", []*gen.PlanStep{step("q", "", "", "", false)}, nil, nil); y != "" {
		t.Errorf("no matched steps must yield empty skeleton, got:\n%s", y)
	}
	if y := renderSkeleton("t", nil, nil, nil); y != "" {
		t.Errorf("nil steps must yield empty skeleton, got %q", y)
	}

	steps := []*gen.PlanStep{
		step("fetch a", "FetchPage", "h/fetch-tools", "1.0.0", true),
		step("fetch b", "FetchPage", "h/fetch-tools", "1.0.0", true),
	}
	// No bridge verdicts at all (defensive path): the second pick cannot be
	// wired, so it degrades to a commented stub with a deduped alias.
	y := renderSkeleton("t", steps, nil, skPorts(t))
	if !strings.Contains(y, "- alias: fetch_page\n") || !strings.Contains(y, "- alias: fetch_page_2\n") {
		t.Errorf("alias dedupe failed:\n%s", y)
	}
	assertYAMLFlow(t, y, 1, 1)
}

func TestRenderSkeleton_UnresolvedFirstPick(t *testing.T) {
	steps := []*gen.PlanStep{step("fetch", "FetchPage", "h/fetch-tools", "1.0.0", true)}
	ports := map[string]*nodePorts{
		"h/fetch-tools/FetchPage@1.0.0": {ref: "h/fetch-tools/FetchPage@1.0.0", err: "package detail fetch failed"},
	}
	y := renderSkeleton("t", steps, nil, ports)
	if !strings.Contains(y, "could not be mirrored") || strings.Contains(y, "input_facade:") {
		t.Errorf("unresolved first pick must skip the facade with a TODO:\n%s", y)
	}
	if !strings.Contains(y, "edges: []") {
		t.Errorf("no wireable edges must render as edges: []:\n%s", y)
	}
	assertYAMLFlow(t, y, 1, 0)
}

// assertYAMLFlow parses the skeleton as YAML and checks the structural core.
func assertYAMLFlow(t *testing.T, y string, wantNodes, wantEdges int) {
	t.Helper()
	var doc struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Nodes       []struct {
			Alias   string `yaml:"alias"`
			Package string `yaml:"package"`
			Node    string `yaml:"node"`
		} `yaml:"nodes"`
		Edges []struct {
			From    string            `yaml:"from"`
			To      string            `yaml:"to"`
			Adapter map[string]string `yaml:"adapter"`
		} `yaml:"edges"`
		InputFacade map[string]any `yaml:"input_facade"`
	}
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("skeleton is not valid YAML: %v\n%s", err, y)
	}
	if doc.Name == "" || doc.Description == "" {
		t.Errorf("skeleton missing name/description:\n%s", y)
	}
	if len(doc.Nodes) != wantNodes {
		t.Errorf("skeleton has %d nodes, want %d:\n%s", len(doc.Nodes), wantNodes, y)
	}
	if len(doc.Edges) != wantEdges {
		t.Errorf("skeleton has %d edges, want %d:\n%s", len(doc.Edges), wantEdges, y)
	}
	for _, e := range doc.Edges {
		if e.From == "" || e.To == "" {
			t.Errorf("edge missing from/to: %+v", e)
		}
	}
}
