package nodes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// CheckBridges verifies that the picks of an assembled plan can actually be wired together: for every adjacent pair of matched steps it fetches both picked nodes' published proto schemas from the public registry and renders a type-bridge verdict — "bridged" when a shared typed carrier (URL, IBAN, timestamp, lat/lon, ...) connects an output leaf of the first pick to a top-level input of the second (strong positive evidence, named in via), "plausible" when no carrier matches but the types overlap so nothing rules the edge out, "blocked" when an edge is structurally impossible (the consumer exposes no top-level scalar input a foreign node can feed, or the type sets are disjoint), and "unknown" when a schema could not be fetched or parsed. The plan passes through otherwise unchanged; feasible is re-derived as matched-AND-not-blocked (a "plausible" or "unknown" pair does NOT flip feasible — absence of carrier evidence is weak evidence), bridge_status summarizes the whole plan, and every blocked/unknown pair is attributed in error as "bridge: ...". A pair separated by an unmatched step is not checked — the missing node's types are unknown. Whenever at least one step matched, skeleton_yaml carries a draft flow.yaml starting point rendered from the picks and verdicts: nodes, proposed edges (carrier-evidenced adapters where a pair is "bridged", TODO(planner)-marked type-compatible proposals where only "plausible"), an input_facade stub mirroring the first pick's published input fields, and comments marking gaps and blocked edges — feed it to `axiom flow validate` and refine. The optional task field only names/documents that skeleton. Reads only the public registry; no secrets.
func CheckBridges(ctx context.Context, ax axiom.Context, input *gen.CheckBridgesInput) (*gen.PlanResult, error) {
	out := &gen.PlanResult{
		Feasible:     input.Feasible,
		PlanBasis:    input.PlanBasis,
		MatchedCount: input.MatchedCount,
		StepCount:    input.StepCount,
		Coverage:     input.Coverage,
		Steps:        input.Steps,
		Gaps:         input.Gaps,
		Error:        input.Error,
		ScoreBasis:   input.GetScoreBasis(),
	}
	// A caller that hand-builds the input may omit the summary basis; the
	// steps still carry their own, so re-derive rather than report nothing.
	if out.ScoreBasis == "" {
		out.ScoreBasis = aggregateBasis(input.GetSteps())
	}
	// plan_basis is a never-empty sentinel on every PlanResult this package
	// emits; a caller invoking CheckBridges directly with a hand-built input
	// may omit it, so restore the invariant rather than propagate "".
	if out.PlanBasis == "" {
		out.PlanBasis = "none"
	}

	// Matched picks in step order — the skeleton needs every one of them (and
	// the first one's input schema for the facade stub), not just the paired
	// ones. Nothing matched ⇒ nothing to fetch, no pairs, no skeleton.
	var matched []*gen.PlanStep
	for _, s := range input.Steps {
		if s != nil && s.Matched {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		out.BridgeStatus = "no_pairs"
		return out, nil
	}

	// Adjacent matched pairs, in step order. A gap between two matched steps
	// breaks adjacency: the plan as written routes through a node that does
	// not exist, so any verdict across it would be fiction.
	type pair struct{ from, to *gen.PlanStep }
	var pairs []pair
	for i := 0; i+1 < len(input.Steps); i++ {
		a, b := input.Steps[i], input.Steps[i+1]
		if a != nil && b != nil && a.Matched && b.Matched {
			pairs = append(pairs, pair{a, b})
		}
	}

	// Fetch each distinct package@version once, concurrently (bounded).
	type fetchResult struct {
		detail *pkgDetail
		err    error
	}
	fetches := make(map[string]*fetchResult)
	for _, s := range matched {
		fetches[s.Package+"@"+s.Version] = nil
	}
	keys := make([]string, 0, len(fetches))
	for k := range fetches {
		keys = append(keys, k)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, key := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pkg, version, _ := strings.Cut(key, "@")
			d, err := fetchPackageDetail(ctx, pkg, version)
			mu.Lock()
			fetches[key] = &fetchResult{detail: d, err: err}
			mu.Unlock()
		}(key)
	}
	wg.Wait()

	// Resolve each distinct pick's ports once (schema parsing is pure).
	ports := make(map[string]*nodePorts)
	portsFor := func(s *gen.PlanStep) *nodePorts {
		ref := s.Package + "/" + s.Node + "@" + s.Version
		if np, ok := ports[ref]; ok {
			return np
		}
		fr := fetches[s.Package+"@"+s.Version]
		var np *nodePorts
		if fr == nil || fr.err != nil {
			np = &nodePorts{ref: ref, err: "package detail fetch failed"}
			if fr != nil {
				np.err = "package detail fetch failed: " + fr.err.Error()
			}
		} else {
			np = resolvePorts(fr.detail, ref, s.Node)
		}
		ports[ref] = np
		return np
	}

	var errParts []string
	if out.Error != "" {
		errParts = append(errParts, out.Error)
	}
	anyBlocked, anyUnknown, anyPlausible := false, false, false
	for _, p := range pairs {
		bc := judgeBridge(portsFor(p.from), portsFor(p.to))
		out.Bridges = append(out.Bridges, bc)
		switch bc.Verdict {
		case "blocked":
			anyBlocked = true
			errParts = append(errParts, fmt.Sprintf("bridge: %s -> %s: %s", bc.FromNode, bc.ToNode, bc.Detail))
		case "unknown":
			anyUnknown = true
			errParts = append(errParts, fmt.Sprintf("bridge: %s -> %s: %s", bc.FromNode, bc.ToNode, bc.Detail))
		case "plausible":
			anyPlausible = true
		}
	}

	switch {
	case len(pairs) == 0:
		out.BridgeStatus = "no_pairs"
	case anyBlocked:
		out.BridgeStatus = "blocked"
		out.Feasible = false
	case anyUnknown:
		out.BridgeStatus = "unknown"
	case anyPlausible:
		out.BridgeStatus = "plausible"
	default:
		out.BridgeStatus = "bridged"
	}
	out.Error = strings.Join(errParts, "; ")

	// v2: the draft flow.yaml. Resolve every matched pick's ports first (the
	// pair loop above only touched the paired ones).
	for _, s := range matched {
		portsFor(s)
	}
	out.SkeletonYaml = renderSkeleton(input.Task, input.Steps, out.Bridges, ports)
	return out, nil
}
