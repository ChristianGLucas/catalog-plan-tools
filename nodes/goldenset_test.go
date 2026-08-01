package nodes_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// goldenCase mirrors testdata/golden.json. The same file drives the LIVE
// oracle (testdata/run_golden_live.py), so this test guards the contract
// between them: a case the runner cannot interpret must fail here, offline,
// rather than silently score as a miss against the deployed node.
type goldenCase struct {
	ID            string `json:"id"`
	Category      string `json:"category"`
	Query         string `json:"query"`
	ExpectPackage string `json:"expect_package"`
	ExpectNode    string `json:"expect_node"`
	ExpectNoMatch bool   `json:"expect_no_match"`
	Note          string `json:"note"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	raw, err := os.ReadFile("../testdata/golden.json")
	if err != nil {
		t.Fatalf("the golden set must be committed alongside the package: %v", err)
	}
	var doc struct {
		Cases []goldenCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("golden.json must stay machine-readable — the live oracle reads it: %v", err)
	}
	return doc.Cases
}

// The accuracy claim in the report is only as good as the set behind it, so
// the set's shape is pinned: enough cases, every category the brief requires,
// and no case that is accidentally unfalsifiable.
func TestGoldenSet_CoversEveryRequiredCategory(t *testing.T) {
	cases := loadGolden(t)
	if len(cases) < 12 {
		t.Fatalf("the golden set must carry at least 12 cases, got %d", len(cases))
	}

	byCategory := map[string]int{}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("every case needs a unique id (offending: %q)", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Query) == "" {
			t.Fatalf("%s: a case with no query scores nothing", c.ID)
		}
		if strings.TrimSpace(c.Note) == "" {
			t.Fatalf("%s: every case must say WHY it discriminates, or it is untrustworthy evidence", c.ID)
		}
		// Exactly one kind of expectation, or the runner cannot judge it.
		hasPick := c.ExpectPackage != "" && c.ExpectNode != ""
		if hasPick == c.ExpectNoMatch {
			t.Fatalf("%s: a case must expect EITHER a specific pick OR no match, not both/neither", c.ID)
		}
		byCategory[c.Category]++
	}

	// The brief's required coverage, each for a reason:
	//   known-failure   the live defect that motivated the upgrade
	//   synonym-recall  what semantic retrieval is FOR
	//   discrimination  what the judge is FOR
	//   wrong-domain-trap  what lexical overlap gets wrong
	//   no-real-match   the calibration guard against over-matching
	//   plain           the no-regression floor
	required := map[string]int{
		"known-failure":     1,
		"synonym-recall":    2,
		"wrong-domain-trap": 2,
		"no-real-match":     2,
		"plain":             3,
		"discrimination":    1,
	}
	for cat, min := range required {
		if byCategory[cat] < min {
			t.Fatalf("category %q needs at least %d cases, got %d", cat, min, byCategory[cat])
		}
	}
}
