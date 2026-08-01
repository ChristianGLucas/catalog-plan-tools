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
	if cs := classifyCarriers("download_url", "string", ""); len(cs) == 0 || cs[0] != "url" {
		t.Errorf("name-based url carrier missed: %v", cs)
	}
	// Description evidence counts only on a compatible JSON type.
	if cs := classifyCarriers("value", "string", "the IBAN to validate"); len(cs) != 1 || cs[0] != "iban" {
		t.Errorf("desc-based iban carrier missed: %v", cs)
	}
	if cs := classifyCarriers("value", "integer", "the IBAN to validate"); len(cs) != 0 {
		t.Errorf("desc match must be type-gated, got %v", cs)
	}
	if cs := classifyCarriers("note", "string", "freeform text about anything"); len(cs) != 0 {
		t.Errorf("freeform text must earn no carrier, got %v", cs)
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
