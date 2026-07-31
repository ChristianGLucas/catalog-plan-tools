package nodes

// Test-only exports: let external (nodes_test) tests point the search client
// at an httptest server and unit-test the defensive JSON parsing.
var (
	SetSearchBaseForTest = func(u string) { searchBase = u }
	ParseQueriesJSON     = parseQueriesJSON
	Tokenize             = tokenize
)
