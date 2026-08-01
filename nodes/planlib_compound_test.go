package nodes

import "testing"

// The real (abridged) description that produced the R15 residual MAJOR: for
// the query "generate a qr code", "qr" appears in this candidate ONLY inside
// the compounds "QR-IBAN"/"QR-IBANs"/"reject_qr_iban", yet it used to earn
// full domain credit and disarm the generic-only halving (score 0.79).
const validateIbanDesc = `Validate an IBAN's country-specific length, BBAN character structure, ` +
	`any country-specific internal check digit, and the mod-97-10 (ISO 7064) IBAN checksum. ` +
	`Returns valid=true/false plus every specific error code and a human-readable reason. ` +
	`Set reject_qr_iban=true to also reject an otherwise-valid Swiss/Liechtenstein QR-IBAN.`

func TestScoreCandidate_CompoundOnlyDomainMatchIsDemoted(t *testing.T) {
	queryToks := Tokenize("generate a qr code")
	iban := apiNode{
		NodeName:    "ValidateIban",
		PackageName: "christiangeorgelucas/iban-tools",
		Version:     "0.2.0",
		Description: validateIbanDesc,
	}
	got := ScoreCandidate(queryToks, iban)
	// "qr" demoted to generic weight (all occurrences are foreign-compound
	// bound), "code" generic, "generate" unmatched, halving fires because no
	// domain word truly matched: (0.35+0.35)/1.7 * 0.5 ≈ 0.206.
	if got >= 0.45 {
		t.Fatalf("ValidateIban must not clear the match threshold for a QR-code query, got %.4f", got)
	}
	if got >= 0.25 {
		t.Fatalf("halving must stay armed when the only domain match is compound-bound, got %.4f", got)
	}

	// The correct pick is untouched: "qr" occurs standalone ("QR code").
	qr := apiNode{
		NodeName:    "GenerateQR",
		PackageName: "christiangeorgelucas/qr-tools",
		Description: "Generate a QR code image (PNG) from text or a URL.",
	}
	if got := ScoreCandidate(queryToks, qr); got != 1.0 {
		t.Fatalf("GenerateQR should score 1.0 on its own query, got %.4f", got)
	}
}

func TestScoreCandidate_CompoundCorroboratedByQueryKeepsCredit(t *testing.T) {
	// When the query itself names the compound's other half, the compound IS
	// the domain being asked about — no demotion.
	queryToks := Tokenize("validate a swiss qr-iban")
	iban := apiNode{
		NodeName:    "ValidateIban",
		PackageName: "christiangeorgelucas/iban-tools",
		Description: validateIbanDesc,
	}
	got := ScoreCandidate(queryToks, iban)
	if got < 0.45 {
		t.Fatalf("qr-iban query must still match the IBAN validator, got %.4f", got)
	}
}

func TestScoreCandidate_NumericAndGenericQualifiersStayClean(t *testing.T) {
	// A digits-only compound neighbor ("utf-8") or a generic one ("qr-code")
	// is a qualifier, not a foreign domain — full credit stands.
	utf := apiNode{
		NodeName:    "EncodeText",
		PackageName: "text-tools",
		Description: "Encode text to UTF-8 bytes.",
	}
	if got := ScoreCandidate(Tokenize("encode text as utf-8"), utf); got != 1.0 {
		t.Fatalf("utf-8 numeric compound must keep full credit, got %.4f", got)
	}
	qrc := apiNode{
		NodeName:    "ScanImage",
		PackageName: "scan-tools",
		Description: "Decode a QR-code from an image.",
	}
	if got := ScoreCandidate(Tokenize("scan a qr image"), qrc); got < 0.45 {
		t.Fatalf("generic compound neighbor (qr-code) must not demote, got %.4f", got)
	}
}

func TestScoreCandidate_PackageNameHyphensAreSeparators(t *testing.T) {
	// "iban-tools" is naming convention: the "iban" occurrence in the package
	// name alone must count as a clean standalone match.
	n := apiNode{
		NodeName:    "Lookup",
		PackageName: "someone/iban-tools",
		Description: "Bank identity lookup.",
	}
	got := ScoreCandidate(Tokenize("iban bank lookup"), n)
	if got < 0.45 {
		t.Fatalf("package-name hyphen must not trigger the compound gate, got %.4f", got)
	}
}

func TestTokenRunsAndCompoundOnly(t *testing.T) {
	runs := TokenRuns("Set reject_qr_iban=true; see QR-IBANs and QR codes, UTF-8 too. GenerateQR.")
	q := map[string]bool{"qr": true, "code": true, "generate": true}
	// "qr" has standalone occurrences ("QR codes", camel-split "GenerateQR").
	if CompoundOnly(runs, "qr", q) {
		t.Fatal("qr has clean occurrences here; compoundOnly must be false")
	}
	// "iban" occurs only joined to qr (in-query → corroborated, so clean).
	if CompoundOnly(runs, "iban", q) {
		t.Fatal("iban joined to an in-query token is corroborated, not foreign")
	}
	// With qr NOT in the query, iban's only occurrences are foreign-compound.
	if !CompoundOnly(runs, "iban", map[string]bool{"bank": true}) {
		t.Fatal("iban occurs only inside reject_qr_iban / QR-IBANs; must be compound-only")
	}
	// Token absent entirely → not compound-only (found=false).
	if CompoundOnly(runs, "zebra", q) {
		t.Fatal("absent token must report false")
	}
}

// The v1 calibration queries must be unaffected (no regression).
func TestScoreCandidate_V1ReprosUnchanged(t *testing.T) {
	iban := apiNode{
		NodeName:    "ValidateIban",
		PackageName: "christiangeorgelucas/iban-tools",
		Description: validateIbanDesc,
	}
	if got := ScoreCandidate(Tokenize("validate iban checksum"), iban); got < 0.9 {
		t.Fatalf("on-domain IBAN query regressed to %.4f", got)
	}
	pharma := apiNode{
		NodeName:    "GetPackaging",
		PackageName: "dailymed-connector",
		Description: "Look up drug packaging details by product code.",
	}
	if got := ScoreCandidate(Tokenize("look up bank details by code"), pharma); got >= 0.45 {
		t.Fatalf("generic-overlap pharma candidate must stay below threshold, got %.4f", got)
	}
}
