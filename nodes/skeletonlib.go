package nodes

// skeletonlib: renders PlanResult.skeleton_yaml — a draft flow.yaml starting
// point built from the matched picks, their fetched schemas, and the bridge
// verdicts. The skeleton is deliberately a STARTING POINT: every proposed
// adapter is either carrier-evidenced (from a "bridged" verdict's via pairing)
// or explicitly marked TODO(planner); gaps and blocked edges surface as
// comments rather than being silently dropped. The output must stay valid
// against `axiom flow validate` — that is asserted by the planner's own
// oracle, so dialect drift is caught at planner-test time (design §8.5).

import (
	"fmt"
	"regexp"
	"strings"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)
var wsRe = regexp.MustCompile(`\s+`)
var aliasCleanRe = regexp.MustCompile(`[^a-z0-9_]`)

// slugify derives the skeleton's flow name from the task text.
func slugify(task string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(task), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
		if i := strings.LastIndex(s, "-"); i > 24 {
			s = s[:i]
		}
	}
	if s == "" {
		return "planned-flow"
	}
	return s
}

// pascal turns a slug into a PascalCase identifier for the facade message name.
func pascal(slug string) string {
	var b strings.Builder
	for _, w := range strings.Split(slug, "-") {
		if w == "" {
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	if b.Len() == 0 {
		return "Planned"
	}
	return b.String()
}

// yq renders s as a single-quoted YAML scalar.
func yq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// collapse folds all whitespace runs (incl. newlines) into single spaces so
// task text and proto comments stay on one YAML line.
func collapse(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// aliasFor derives a unique snake_case node alias from a node name.
func aliasFor(node string, used map[string]int) string {
	base := aliasCleanRe.ReplaceAllString(
		strings.ReplaceAll(strings.ToLower(camelBoundary(node)), " ", "_"), "")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "node"
	}
	used[base]++
	if used[base] > 1 {
		return fmt.Sprintf("%s_%d", base, used[base])
	}
	return base
}

// facadeKind maps a raw proto scalar type to a facade field kind. Enum and
// otherwise-unresolvable named types serialize as strings in JSON.
func facadeKind(ptyp string) string {
	switch ptyp {
	case "string", "bool", "bytes", "int32", "int64", "uint32", "uint64", "double", "float":
		return ptyp
	case "sint32", "sfixed32":
		return "int32"
	case "sint64", "sfixed64":
		return "int64"
	case "fixed32":
		return "uint32"
	case "fixed64":
		return "uint64"
	}
	return "string"
}

// parseVia inverts judgeBridge's via format "<carrier>: <outPath> -> <inField>".
// Keep in sync with the fmt.Sprintf in judgeBridge (a round-trip unit test
// pins the pairing).
func parseVia(via string) (carrier, outPath, inField string, ok bool) {
	carrier, rest, found := strings.Cut(via, ": ")
	if !found {
		return "", "", "", false
	}
	outPath, inField, found = strings.Cut(rest, " -> ")
	if !found {
		return "", "", "", false
	}
	return carrier, outPath, inField, true
}

// proposePairing picks a deterministic type-compatible (output leaf, input
// field) pairing for a pair with no carrier evidence: the shortest adapter-
// pickable producer path (ties broken lexicographically) against the first
// compatible consumer field in declared order. Paths through repeated
// messages ("[]") and the bare "error" leaf are excluded — neither is a
// sensible plain edge pick.
func proposePairing(from, to *nodePorts) (outPath, inField string, ok bool) {
	for _, ol := range from.outLeafs {
		if ol.path == "error" || strings.Contains(ol.path, "[]") {
			continue
		}
		for _, inf := range to.inFields {
			if !jtypesCompatible(ol.jtype, inf.jtype) {
				continue
			}
			if !ok || len(ol.path) < len(outPath) || (len(ol.path) == len(outPath) && ol.path < outPath) {
				outPath, inField, ok = ol.path, inf.path, true
			}
			break // first compatible consumer field in declared order
		}
	}
	return outPath, inField, ok
}

// renderSkeleton renders the draft flow.yaml. steps is the full plan (matched
// and unmatched, in order), bridges the CheckBridges verdicts for adjacent
// matched pairs, ports the resolved schema surface per pick ref. Returns ""
// when no step matched.
func renderSkeleton(task string, steps []*gen.PlanStep, bridges []*gen.BridgeCheck, ports map[string]*nodePorts) string {
	type pick struct {
		step    *gen.PlanStep
		stepIdx int
		alias   string
		ref     string
	}
	var picks []pick
	used := map[string]int{}
	for i, s := range steps {
		if s == nil || !s.Matched {
			continue
		}
		picks = append(picks, pick{
			step: s, stepIdx: i,
			alias: aliasFor(s.Node, used),
			ref:   s.Package + "/" + s.Node + "@" + s.Version,
		})
	}
	if len(picks) == 0 {
		return ""
	}

	bridgeFor := map[string]*gen.BridgeCheck{}
	for _, bc := range bridges {
		if bc != nil {
			bridgeFor[bc.FromNode+"|"+bc.ToNode] = bc
		}
	}

	slug := slugify(task)
	taskLine := collapse(task)
	if taskLine == "" {
		taskLine = "(no task text supplied to the planner)"
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# Draft flow skeleton generated by flow-planner — a STARTING POINT, not a finished flow.")
	w("# Before compiling: verify every edge adapter (especially TODO(planner) markers), add")
	w("# @flow_input edges for any interior-node fields the caller should supply, and add an")
	w("# output_facade if you want a response shape other than the terminal node's output message.")
	w("# TODO(planner): replace \"your-handle\" below with your marketplace handle before saving.")
	w("name: your-handle/%s", slug)
	w("version: 0.1.0")
	w("description: %s", yq(taskLine+" (draft skeleton generated by flow-planner; verify adapters before use)"))

	// --- input facade: mirror the first pick's published input fields ---
	first := picks[0]
	firstPorts := ports[first.ref]
	haveFacade := firstPorts != nil && firstPorts.err == "" && len(firstPorts.inFields) > 0
	if haveFacade {
		w("input_facade:")
		w("    message_name: %sInput", pascal(slug))
		w("    description: %s", yq("Caller inputs, mirrored from "+first.ref+"'s input message — rename, trim, and re-describe as the flow's own contract."))
		w("    fields:")
		for _, f := range firstPorts.inFields {
			w("        %s:", f.path)
			w("            kind: %s", facadeKind(f.ptyp))
			if f.repeated {
				w("            repeated: true")
			}
			desc := collapse(f.desc)
			if desc == "" {
				desc = "TODO(planner): describe this field (mirrored from " + first.step.Node + " input \"" + f.path + "\" — the published field has no description)"
			}
			w("            description: %s", yq(desc))
		}
	} else {
		w("# TODO(planner): %s's input schema could not be mirrored (%s) — declare the", first.ref, portsProblem(firstPorts))
		w("# input_facade (and the '@flow_input' edge below) by hand from `axiom inspect node %s/%s`.", first.step.Package, first.step.Node)
	}

	// --- per-pair wiring decisions. A flow must validate with exactly one
	// terminal node, so only the maximal WIRED PREFIX of picks becomes real
	// nodes: at the first pair with no real proposable edge (a gap, a blocked
	// or unverified verdict, or no plain pick), every later pick is rendered
	// as a commented-out stub the author restores once the break is fixed.
	type edgePlan struct {
		wired            bool
		comment          []string
		inField, outPath string
	}
	plans := make([]edgePlan, 0, max(0, len(picks)-1))
	for k := 0; k+1 < len(picks); k++ {
		a, c := picks[k], picks[k+1]
		var ep edgePlan
		bc := bridgeFor[a.ref+"|"+c.ref]
		switch {
		case c.stepIdx != a.stepIdx+1:
			ep.comment = []string{
				fmt.Sprintf("planner: %s and %s are separated by unmatched step(s) — no edge proposed", a.alias, c.alias),
				"across the gap; close the gap (or wire around it) and restore the stubs below.",
			}
		case bc == nil:
			// Defensive: a pair the bridge pass did not judge.
			ep.comment = []string{fmt.Sprintf("TODO(planner): no bridge verdict for %s -> %s — wire this edge manually.", a.alias, c.alias)}
		case bc.Verdict == "bridged":
			if carrier, outPath, inField, ok := parseVia(bc.Via); ok && !strings.Contains(outPath, "[]") {
				ep = edgePlan{wired: true, inField: inField, outPath: outPath,
					comment: []string{fmt.Sprintf("planner: carrier %q evidence connects these picks (verdict \"bridged\").", carrier)}}
			} else if outPath, inField, ok2 := proposePairing(ports[a.ref], ports[c.ref]); ok2 {
				// The carrier pairing routes through a repeated message — an
				// adapter can't plain-pick it; fall back to a proposed pairing.
				ep = edgePlan{wired: true, inField: inField, outPath: outPath, comment: []string{
					fmt.Sprintf("TODO(planner): the carrier pairing (%s) crosses a repeated message and cannot", bc.Via),
					"be a plain pick — this pairing is only type-compatible, verify it:"}}
			} else {
				ep.comment = []string{
					fmt.Sprintf("TODO(planner): carrier evidence exists (%s) but no plain adapter pick is", bc.Via),
					fmt.Sprintf("possible — wire %s -> %s manually (a CEL/map adapter may be needed).", a.alias, c.alias)}
			}
		case bc.Verdict == "plausible":
			if outPath, inField, ok := proposePairing(ports[a.ref], ports[c.ref]); ok {
				ep = edgePlan{wired: true, inField: inField, outPath: outPath, comment: []string{
					"TODO(planner): no carrier evidence for this edge (verdict \"plausible\") —",
					"this pairing is only type-compatible, verify it against both schemas:"}}
			} else {
				ep.comment = []string{fmt.Sprintf("TODO(planner): edge %s -> %s is type-plausible but no plain pick could be proposed — wire it manually.", a.alias, c.alias)}
			}
		case bc.Verdict == "blocked":
			ep.comment = []string{
				fmt.Sprintf("planner: edge %s -> %s is BLOCKED — %s.", a.alias, c.alias, collapse(bc.Detail)),
				"An edge adapter cannot connect these picks; swap one of them (see the plan's",
				"alternatives) before this flow can compile."}
		default: // "unknown"
			ep.comment = []string{fmt.Sprintf("TODO(planner): %s -> %s could not be verified (%s) — wire it manually.", a.alias, c.alias, collapse(bc.Detail))}
		}
		plans = append(plans, ep)
	}
	wiredPairs := 0
	for wiredPairs < len(plans) && plans[wiredPairs].wired {
		wiredPairs++
	}
	realPicks := wiredPairs + 1 // picks[0..realPicks-1] are real nodes

	// --- nodes: the wired prefix as real entries; unmatched steps and
	// beyond-the-break picks surface as comments ---
	w("nodes:")
	pi := 0
	for i, s := range steps {
		if s == nil || !s.Matched {
			q := ""
			if s != nil {
				q = collapse(s.Description)
			}
			w("    # planner: step %d (%s) has NO catalog match — a gap. Close it (see gaps[] /", i+1, yq(q))
			w("    # `axiom propose`) or wire around it; the proposed chain breaks here.")
			continue
		}
		if pi == realPicks && pi < len(picks) {
			w("    # planner: the picks below could not be wired to the chain above (see the edge")
			w("    # comments) — they are stubs so the draft validates; restore them as you wire them.")
		}
		pfx, ppfx := "    ", "      "
		if pi >= realPicks {
			pfx, ppfx = "    # ", "    #   "
		}
		w("%s- alias: %s", pfx, picks[pi].alias)
		w("%spackage: %s@%s", ppfx, s.Package, s.Version)
		w("%snode: %s", ppfx, s.Node)
		w("%scol: %d", ppfx, pi)
		w("%srow: 0", ppfx)
		pi++
	}

	// --- edges (buffered: a comments-only section must render as `edges: []`,
	// never a bare `edges:` key with a null value) ---
	var eb strings.Builder
	w = func(format string, args ...any) { fmt.Fprintf(&eb, format+"\n", args...) }
	if haveFacade {
		w("    - from: '@flow_input'")
		w("      to: %s", first.alias)
		w("      adapter:")
		for _, f := range firstPorts.inFields {
			w("        %s: %s", f.path, f.path)
		}
	}
	for k, ep := range plans {
		a, c := picks[k], picks[k+1]
		for _, line := range ep.comment {
			w("    # %s", line)
		}
		pfx, ppfx := "    ", "      "
		if !ep.wired || k >= wiredPairs {
			// Beyond the break (even a wired-looking later pair targets a stub
			// node) — emit the whole edge commented out.
			pfx, ppfx = "    # ", "    #   "
		}
		if ep.wired {
			w("%s- from: %s", pfx, a.alias)
			w("%sto: %s", ppfx, c.alias)
			w("%sadapter:", ppfx)
			w("%s    %s: %s", ppfx, ep.inField, ep.outPath)
		} else {
			w("%s- from: %s", pfx, a.alias)
			w("%sto: %s", ppfx, c.alias)
			w("%sadapter: {} # TODO(planner): map the fields", ppfx)
		}
	}

	edgeBody := eb.String()
	if strings.Count("\n"+edgeBody, "\n    - from:") == 0 {
		b.WriteString("edges: []\n")
	} else {
		b.WriteString("edges:\n")
	}
	b.WriteString(edgeBody)
	return b.String()
}

func portsProblem(np *nodePorts) string {
	if np == nil {
		return "schema not resolved"
	}
	if np.err != "" {
		return collapse(np.err)
	}
	return "the node declares no top-level scalar inputs"
}
