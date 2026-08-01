package nodes

// Test-only exports: let external (nodes_test) tests point the HTTP clients
// at httptest servers and unit-test the defensive parsing and scoring.
var (
	SetSearchBaseForTest = func(u string) { searchBase = u }
	SetPlanBaseForTest   = func(u string) { planBase = u }
	ParseQueriesJSON     = parseQueriesJSON
	Tokenize             = tokenize
	ScoreCandidate       = scoreCandidate
	TokenRuns            = tokenRuns
	CompoundOnly         = compoundOnly
)

// APINode re-exports the search-response row type for scoring tests.
type APINode = apiNode
