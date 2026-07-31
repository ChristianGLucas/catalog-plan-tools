package nodes_test

import (
	"context"
	"strings"
	"testing"

	"christiangeorgelucas/catalog-plan-tools/axiom"
	gen "christiangeorgelucas/catalog-plan-tools/gen"
	"christiangeorgelucas/catalog-plan-tools/nodes"
)

// testContext is a testing.T-backed axiom.Context for unit tests.
type testContext struct {
	t            *testing.T
	secretsMap   map[string]string
	revokedNames map[string]bool
}

func newTestContext(t *testing.T) *testContext {
	return &testContext{t: t, secretsMap: map[string]string{}, revokedNames: map[string]bool{}}
}

type testLogger struct{ t *testing.T }

func (l *testLogger) Debug(msg string, args ...any) { l.t.Logf("DEBUG  %s %v", msg, args) }
func (l *testLogger) Info(msg string, args ...any)  { l.t.Logf("INFO   %s %v", msg, args) }
func (l *testLogger) Warn(msg string, args ...any)  { l.t.Logf("WARN   %s %v", msg, args) }
func (l *testLogger) Error(msg string, args ...any) { l.t.Logf("ERROR  %s %v", msg, args) }

type testSecrets struct {
	m       map[string]string
	revoked map[string]bool
}

func (s testSecrets) Get(name string) (string, bool) { v, ok := s.m[name]; return v, ok }

func (s testSecrets) Status(name string) axiom.SecretStatus {
	if _, ok := s.m[name]; ok {
		return axiom.SecretStatusAvailable
	}
	if s.revoked[name] {
		return axiom.SecretStatusRevoked
	}
	return axiom.SecretStatusUnset
}

type testFlowReflection struct{}

func (testFlowReflection) Nodes() []axiom.ReflectionNode     { return nil }
func (testFlowReflection) Edges() []axiom.ReflectionEdge     { return nil }
func (testFlowReflection) LoopEdges() []axiom.ReflectionEdge { return nil }
func (testFlowReflection) Position() axiom.FlowPosition      { return axiom.FlowPosition{} }
func (testFlowReflection) GraphID() string                   { return "" }

type testReflection struct{}

func (testReflection) Flow() axiom.FlowReflection { return testFlowReflection{} }

type testFlowMutation struct{}

func (testFlowMutation) AddNode(_, _ string, _ *axiom.CanvasPosition) uint32 { return 0 }
func (testFlowMutation) AddEdge(_, _ uint32, _ *axiom.EdgeCondition)         {}

type testMutation struct{}

func (testMutation) Flow() axiom.FlowMutation { return testFlowMutation{} }

func (c *testContext) Log() axiom.Logger            { return &testLogger{c.t} }
func (c *testContext) Secrets() axiom.Secrets       { return testSecrets{c.secretsMap, c.revokedNames} }
func (c *testContext) ExecutionID() string          { return "test-execution-id" }
func (c *testContext) FlowID() string               { return "test-flow-id" }
func (c *testContext) TenantID() string             { return "test-tenant-id" }
func (c *testContext) Reflection() axiom.Reflection { return testReflection{} }
func (c *testContext) Mutation() axiom.Mutation     { return testMutation{} }

var _ axiom.Context = (*testContext)(nil)

func cand(node, pkg, ver string, score float64) *gen.Candidate {
	return &gen.Candidate{Node: node, Package: pkg, Version: ver, Description: node + " does things", Score: score}
}

func TestAssemblePlan_DecomposedAllMatched(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Primary: []*gen.StepCandidates{
			{Query: "validate iban checksum", Candidates: []*gen.Candidate{
				cand("ValidateIban", "h/iban-tools", "0.1.2", 1.0),
				cand("CheckIbanCountry", "h/iban-tools", "0.1.2", 0.6),
			}},
			{Query: "look up bank by iban", Candidates: []*gen.Candidate{
				cand("BankFromIban", "h/bank-tools", "0.2.0", 0.8),
			}},
		},
		Fallback: []*gen.StepCandidates{{Query: "whole description"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Feasible || got.PlanBasis != "decomposed" || got.MatchedCount != 2 || got.StepCount != 2 {
		t.Fatalf("want feasible decomposed 2/2, got %+v", got)
	}
	if got.Coverage != 1.0 {
		t.Errorf("coverage = %v, want 1.0", got.Coverage)
	}
	if got.Error != "" {
		t.Errorf("error should be empty on the clean path, got %q", got.Error)
	}
	if got.Steps[0].Node != "ValidateIban" || got.Steps[0].Package != "h/iban-tools" {
		t.Errorf("best pick wrong: %+v", got.Steps[0])
	}
	if len(got.Steps[0].Alternatives) != 1 || got.Steps[0].Alternatives[0] != "h/iban-tools/CheckIbanCountry@0.1.2" {
		t.Errorf("alternatives wrong: %v", got.Steps[0].Alternatives)
	}
	if len(got.Gaps) != 0 {
		t.Errorf("no gaps expected, got %v", got.Gaps)
	}
}

func TestAssemblePlan_BelowThresholdIsGapWithNearMiss(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Primary: []*gen.StepCandidates{
			{Query: "matched step", Candidates: []*gen.Candidate{cand("Good", "h/p", "1", 0.9)}},
			{Query: "kyc screening", Candidates: []*gen.Candidate{cand("SshConfig", "h/ssh-tools", "1", 0.2)}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Feasible {
		t.Fatal("feasible must be false with an unmatched step")
	}
	if got.MatchedCount != 1 || got.Coverage != 0.5 {
		t.Errorf("matched/coverage = %d/%v, want 1/0.5", got.MatchedCount, got.Coverage)
	}
	if len(got.Gaps) != 1 || got.Gaps[0].Description != "kyc screening" {
		t.Fatalf("want one gap for the unmatched step, got %v", got.Gaps)
	}
	if !strings.Contains(got.Gaps[0].ProposalHint, "h/ssh-tools/SshConfig") {
		t.Errorf("hint should name the near-miss, got %q", got.Gaps[0].ProposalHint)
	}
	// The below-threshold step still reports its best score so callers can inspect the near-miss.
	if got.Steps[1].Score != 0.2 || got.Steps[1].Matched {
		t.Errorf("unmatched step should carry score 0.2, matched=false: %+v", got.Steps[1])
	}
}

func TestAssemblePlan_FallbackBasisAttributesDecomposeError(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Fallback: []*gen.StepCandidates{
			{Query: "the whole task", Candidates: []*gen.Candidate{cand("DoTask", "h/p", "1", 0.7)}},
		},
		LlmError: "HTTP 401: invalid x-api-key",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PlanBasis != "fallback" {
		t.Fatalf("plan_basis = %q, want fallback", got.PlanBasis)
	}
	if !strings.HasPrefix(got.Error, "decompose: HTTP 401") {
		t.Errorf("error must attribute the decompose failure, got %q", got.Error)
	}
	if !got.Feasible || got.MatchedCount != 1 {
		t.Errorf("fallback single-step plan should still be feasible: %+v", got)
	}
}

func TestAssemblePlan_FallbackWithoutLlmErrorStillAttributed(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Fallback: []*gen.StepCandidates{{Query: "q", Candidates: []*gen.Candidate{cand("N", "h/p", "1", 0.9)}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got.Error, "decompose: no decomposition available") {
		t.Errorf("degraded basis must always be attributed, got %q", got.Error)
	}
}

func TestAssemblePlan_NoInputSentinel(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PlanBasis != "none" || got.Error != "NO_INPUT" || got.Feasible {
		t.Fatalf("want explicit none/NO_INPUT/not-feasible, got %+v", got)
	}
}

func TestAssemblePlan_SearchErrorIsNotAGap(t *testing.T) {
	got, err := nodes.AssemblePlan(context.Background(), newTestContext(t), &gen.AssemblePlanInput{
		Primary: []*gen.StepCandidates{
			{Query: "reachable step", Candidates: []*gen.Candidate{cand("N", "h/p", "1", 0.9)}},
			{Query: "unreachable step", Error: "search: HTTP 503: upstream"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Gaps) != 0 {
		t.Fatalf("a transport-failed step must not become a gap, got %v", got.Gaps)
	}
	if got.Feasible || got.MatchedCount != 1 {
		t.Errorf("errored step is unmatched: %+v", got)
	}
	if !strings.Contains(got.Error, "search: unreachable step:") {
		t.Errorf("error must attribute the failed search, got %q", got.Error)
	}
}

func TestAssemblePlan_ThresholdOverride(t *testing.T) {
	in := &gen.AssemblePlanInput{
		Primary: []*gen.StepCandidates{
			{Query: "q", Candidates: []*gen.Candidate{cand("N", "h/p", "1", 0.5)}},
		},
	}
	got, _ := nodes.AssemblePlan(context.Background(), newTestContext(t), in)
	if !got.Feasible {
		t.Fatal("0.5 clears the default threshold 0.45")
	}
	in.Threshold = 0.6
	got, _ = nodes.AssemblePlan(context.Background(), newTestContext(t), in)
	if got.Feasible {
		t.Fatal("0.5 must not clear an explicit threshold of 0.6")
	}
}
