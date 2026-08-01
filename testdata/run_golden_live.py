#!/usr/bin/env python3
"""Run the committed golden set against the DEPLOYED SearchSteps node, once per
scoring stage, and report one accuracy per stage.

This is the live half of the planner's accuracy oracle (the offline half is
nodes/goldenset_test.go, which pins the set's shape). It exists to prove ONE
claim the scoring upgrade rests on:

    accuracy(judge) >= accuracy(semantic) >= accuracy(lexical)

A stage is scored on the deployed node, not on a local reimplementation, so the
numbers describe what a caller of the published planner actually gets.

    python3 testdata/run_golden_live.py --axiom /path/to/axiom [--version 0.7.0]

A case counts as correct when the step's TOP pick is the expected node, or —
for an expect_no_match case — when the step comes back unmatched. Unmatched
positive cases and matched negative cases are both failures: over-matching is
not accuracy, it is a confident liar.
"""

import argparse
import json
import os
import subprocess
import sys

STAGES = ["lexical", "semantic", ""]  # "" = auto (judge, degrading)
STAGE_LABEL = {"lexical": "lexical", "semantic": "semantic", "": "judge (auto)"}


def invoke(axiom, version, query, mode):
    payload = {"queries": [query], "scoring_mode": mode}
    proc = subprocess.run(
        [axiom, "invoke",
         f"christiangeorgelucas/catalog-plan-tools/SearchSteps@{version}",
         "--input", json.dumps(payload), "--timeout", "180"],
        capture_output=True, text=True,
    )
    if proc.returncode != 0:
        return None, proc.stderr.strip()[:200]
    for line in proc.stdout.splitlines():
        line = line.strip()
        if line.startswith("{"):
            try:
                return json.loads(line), None
            except json.JSONDecodeError:
                continue
    return None, "no JSON in response"


def verdict(case, result):
    """Return (correct: bool, detail: str) for one case at one stage."""
    steps = (result or {}).get("steps") or []
    if not steps:
        return False, "no step returned"
    step = steps[0]
    basis = step.get("score_basis", "?")
    cands = step.get("candidates") or []
    # The matched verdict is the assemble threshold for THIS basis — mirrored
    # here so the oracle measures the same thing a plan reports.
    thresholds = {"judge": 0.55, "semantic": 0.38, "lexical": 0.45}
    top = cands[0] if cands else None
    score = (top or {}).get("score", 0.0)
    matched = bool(top) and score >= thresholds.get(basis, 0.45)

    if case.get("expect_no_match"):
        ok = not matched
        got = f"{top['package']}/{top['node']}" if top else "(nothing)"
        return ok, f"basis={basis} matched={matched} top={got} score={score:.3f}"

    if not top:
        return False, f"basis={basis} no candidates"
    got = f"{top['package']}/{top['node']}"
    want = f"{case['expect_package']}/{case['expect_node']}"
    ok = got == want and matched
    why = "" if got == want else f" (wanted {want})"
    return ok, f"basis={basis} matched={matched} top={got} score={score:.3f}{why}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--axiom", default=os.environ.get("AXIOM", "axiom"))
    ap.add_argument("--version", default="0.7.0")
    ap.add_argument("--golden", default=os.path.join(os.path.dirname(__file__), "golden.json"))
    args = ap.parse_args()

    cases = json.load(open(args.golden))["cases"]
    accuracy, rows = {}, {}
    for mode in STAGES:
        correct = 0
        rows[mode] = []
        for case in cases:
            result, err = invoke(args.axiom, args.version, case["query"], mode)
            if err:
                ok, detail = False, f"INVOKE FAILED: {err}"
            else:
                ok, detail = verdict(case, result)
            correct += ok
            rows[mode].append((case["id"], case["category"], ok, detail))
            print(f"  [{STAGE_LABEL[mode]:14s}] {case['id']:26s} {'PASS' if ok else 'FAIL'}  {detail}",
                  flush=True)
        accuracy[mode] = correct / len(cases)
        print(f"== {STAGE_LABEL[mode]}: {correct}/{len(cases)} = {accuracy[mode]:.1%}\n", flush=True)

    lex, sem, judge = accuracy["lexical"], accuracy["semantic"], accuracy[""]
    print(f"ACCURACY  lexical={lex:.1%}  semantic={sem:.1%}  judge={judge:.1%}")
    monotone = judge >= sem >= lex
    print(f"MONOTONE (judge >= semantic >= lexical): {'YES' if monotone else 'NO'}")

    json.dump({"accuracy": {STAGE_LABEL[m]: accuracy[m] for m in STAGES},
               "monotone": monotone,
               "rows": {STAGE_LABEL[m]: rows[m] for m in STAGES}},
              open("golden-results.json", "w"), indent=1)
    print("wrote golden-results.json")
    return 0 if monotone else 1


if __name__ == "__main__":
    sys.exit(main())
