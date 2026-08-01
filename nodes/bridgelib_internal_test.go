package nodes

import (
	"strings"
	"testing"
)

const fixtureProto = `
syntax = "proto3";
package test.fixture;

// A fetched release.
message Release {
  // The direct download URL for the artifact.
  string download_url = 1;
  // When the release was published, ISO-8601.
  string published_at = 2;
  Asset primary_asset = 3;
  repeated Asset assets = 4;
  int64 size_bytes = 5;
}

message Asset {
  string asset_url = 1;
  string label = 2;
}

message WrapperOnly {
  Release release = 1;
}

message WithEnumAndOneof {
  enum Kind {
    KIND_UNKNOWN = 0;
    KIND_FULL = 1;
  }
  Kind kind = 1;
  oneof choice {
    // The chosen page address.
    string page_url = 2;
    int32 index = 3;
  }
}
`

func TestParseProtoMessages_FieldsCommentsNesting(t *testing.T) {
	msgs := parseProtoMessages(fixtureProto)
	rel := msgs["Release"]
	if len(rel) != 5 {
		t.Fatalf("Release should have 5 fields, got %d: %+v", len(rel), rel)
	}
	if rel[0].name != "download_url" || rel[0].desc != "The direct download URL for the artifact." {
		t.Errorf("leading comment not captured: %+v", rel[0])
	}
	if !rel[3].repeated || rel[3].typ != "Asset" {
		t.Errorf("repeated message field mis-parsed: %+v", rel[3])
	}
	// Nested enum values must NOT be parsed as fields; oneof members MUST be
	// (they are ordinary fields of the enclosing message).
	var names []string
	for _, f := range msgs["WithEnumAndOneof"] {
		names = append(names, f.name)
	}
	if got := strings.Join(names, ","); got != "kind,page_url,index" {
		t.Errorf("enum/oneof handling wrong, got fields %q", got)
	}
	if len(msgs["Asset"]) != 2 {
		t.Errorf("sibling message lost: %+v", msgs["Asset"])
	}
}

func TestClassifyCarriers(t *testing.T) {
	if cs := classifyCarriers("download_url", "string", "", false); len(cs) == 0 || cs[0] != "url" {
		t.Errorf("name-based url carrier missed: %v", cs)
	}
	// Consumer side (allowDesc): description evidence counts, type-gated.
	if cs := classifyCarriers("value", "string", "the IBAN to validate", true); len(cs) != 1 || cs[0] != "iban" {
		t.Errorf("desc-based iban carrier missed: %v", cs)
	}
	if cs := classifyCarriers("value", "integer", "the IBAN to validate", true); len(cs) != 0 {
		t.Errorf("desc match must be type-gated, got %v", cs)
	}
	if cs := classifyCarriers("note", "string", "freeform text about anything", true); len(cs) != 0 {
		t.Errorf("freeform text must earn no carrier, got %v", cs)
	}
	// A boolean is ABOUT an iban, it does not CARRY one — live false positive:
	// bool iban_valid must never earn the iban carrier (nor any other).
	if cs := classifyCarriers("iban_valid", "boolean", "whether the IBAN is valid", true); len(cs) != 0 {
		t.Errorf("boolean field must earn no carrier, got %v", cs)
	}
	// Producer side (name-only): a description that MENTIONS an IBAN must not
	// award the carrier — second live false positive ("the country code
	// extracted from the IBAN" earned carrier iban for actual_country_code).
	if cs := classifyCarriers("actual_country_code", "string", "The country code extracted from the IBAN, reported even if the IBAN fails validation.", false); len(cs) != 1 || cs[0] != "country_code" {
		t.Errorf("producer desc mention must not award iban, got %v", cs)
	}
	// camelCase field names in real published packages must hit the
	// snake_case name patterns (R15 review MINOR).
	if cs := classifyCarriers("downloadUrl", "string", "", false); len(cs) == 0 || cs[0] != "url" {
		t.Errorf("camelCase name must earn url carrier, got %v", cs)
	}
}

func TestJudgeBridge_TypeGates(t *testing.T) {
	// Producer emits bool iban_valid (+ a plain string); consumer wants a
	// string iban. Must NOT be "bridged" — the observed live false positive.
	prod := mkPorts(t, `
message POut { bool iban_valid = 1; string note = 2; }
message PIn { string q = 1; }
`, "PIn", "POut", "h/p/A@1")
	cons := mkPorts(t, `
message CIn { string iban = 1; }
message COut { string r = 1; }
`, "CIn", "COut", "h/p/B@1")
	if bc := judgeBridge(prod, cons); bc.Verdict != "plausible" {
		t.Errorf("bool iban_valid -> string iban must not bridge, got %+v", bc)
	}

	// Both ends earn "instant", but string ISO output cannot feed an int64
	// unix field without conversion: pairing must be type-compatible.
	isoProd := mkPorts(t, `
message POut { string created_at = 1; }
message PIn { string q = 1; }
`, "PIn", "POut", "h/p/C@1")
	unixCons := mkPorts(t, `
message CIn { int64 timestamp = 1; string label = 2; }
message COut { string r = 1; }
`, "CIn", "COut", "h/p/D@1")
	if bc := judgeBridge(isoProd, unixCons); bc.Verdict == "bridged" {
		t.Errorf("string instant -> integer instant must not claim bridged, got %+v", bc)
	}

	// Live false positive #2: a producer output whose DESCRIPTION mentions an
	// IBAN ("the country code extracted from the IBAN") must not bridge to a
	// string iban input on the iban carrier.
	ccProd := mkPorts(t, `
message POut {
  // The country code extracted from the IBAN, reported even if the IBAN fails validation.
  string actual_country_code = 1;
  bool matches = 2;
}
message PIn { string q = 1; }
`, "PIn", "POut", "h/p/E@1")
	ibanCons := mkPorts(t, `
message CIn {
  // The IBAN to parse.
  string iban = 1;
}
message COut { string r = 1; }
`, "CIn", "COut", "h/p/F@1")
	if bc := judgeBridge(ccProd, ibanCons); bc.Verdict != "plausible" {
		t.Errorf("desc-mention producer must not bridge on iban, got %+v", bc)
	}
}

func TestWalkOutputsAndFeedableInputs(t *testing.T) {
	msgs := parseProtoMessages(fixtureProto)
	leaves := walkOutputs(msgs, "Release")
	paths := map[string]bool{}
	for _, l := range leaves {
		paths[l.path] = true
	}
	for _, want := range []string{"download_url", "published_at", "primary_asset.asset_url", "assets[].label", "size_bytes"} {
		if !paths[want] {
			t.Errorf("missing output leaf %q in %v", want, paths)
		}
	}
	// Consumers: a message-typed field is not feedable by a foreign producer.
	if in := feedableInputs(msgs, "WrapperOnly"); len(in) != 0 {
		t.Errorf("WrapperOnly should expose no feedable inputs, got %v", in)
	}
	if in := feedableInputs(msgs, "Asset"); len(in) != 2 {
		t.Errorf("Asset should expose 2 feedable inputs, got %v", in)
	}
}

func TestWalkOutputs_CycleGuard(t *testing.T) {
	cyclic := `
message A { string a_url = 1; B next = 2; }
message B { A back = 1; string label = 2; }
`
	leaves := walkOutputs(parseProtoMessages(cyclic), "A")
	if len(leaves) == 0 || len(leaves) > 20 {
		t.Fatalf("cycle guard failed, %d leaves", len(leaves))
	}
}

func mkPorts(t *testing.T, src, inMsg, outMsg, ref string) *nodePorts {
	t.Helper()
	msgs := parseProtoMessages(src)
	if _, ok := msgs[inMsg]; !ok {
		t.Fatalf("fixture missing %s", inMsg)
	}
	return &nodePorts{ref: ref, outLeafs: walkOutputs(msgs, outMsg), inFields: feedableInputs(msgs, inMsg)}
}

const producerProto = `
message POut {
  string download_url = 1;
  string body = 2;
}
message PIn { string q = 1; }
`

func TestJudgeBridge_Verdicts(t *testing.T) {
	prod := mkPorts(t, producerProto, "PIn", "POut", "h/p/A@1")

	urlConsumer := mkPorts(t, `
message CIn { string target_url = 1; string label = 2; }
message COut { string result = 1; }
`, "CIn", "COut", "h/p/B@1")
	bridged := judgeBridge(prod, urlConsumer)
	if bridged.Verdict != "bridged" || !bridged.Compatible || bridged.Via != "url: download_url -> target_url" {
		t.Errorf("expected carrier bridge, got %+v", bridged)
	}

	stringConsumer := mkPorts(t, `
message CIn { string note = 1; }
message COut { string result = 1; }
`, "CIn", "COut", "h/p/C@1")
	plaus := judgeBridge(prod, stringConsumer)
	if plaus.Verdict != "plausible" || plaus.Compatible {
		t.Errorf("expected plausible (string overlap, no carrier), got %+v", plaus)
	}

	msgOnlyConsumer := mkPorts(t, `
message Inner { string x = 1; }
message CIn { Inner payload = 1; }
message COut { string result = 1; }
`, "CIn", "COut", "h/p/D@1")
	blocked := judgeBridge(prod, msgOnlyConsumer)
	if blocked.Verdict != "blocked" || blocked.Compatible {
		t.Errorf("expected blocked (no feedable inputs), got %+v", blocked)
	}

	// A producer whose only leaf is "error" has nothing real to offer.
	errOnlyProd := mkPorts(t, `
message POut { string error = 1; }
message PIn { string q = 1; }
`, "PIn", "POut", "h/p/E@1")
	blocked2 := judgeBridge(errOnlyProd, stringConsumer)
	if blocked2.Verdict != "blocked" {
		t.Errorf("error-only producer must be blocked, got %+v", blocked2)
	}

	unknown := judgeBridge(prod, &nodePorts{ref: "h/p/F@1", err: "package detail fetch failed"})
	if unknown.Verdict != "unknown" || unknown.Compatible {
		t.Errorf("expected unknown on unresolved ports, got %+v", unknown)
	}
}
