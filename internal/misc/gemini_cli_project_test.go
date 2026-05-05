package misc

import (
	"strings"
	"testing"
)

func TestResolveGeminiCLIProjectIDPrefersBackendProjectForFreeTier(t *testing.T) {
	got := ResolveGeminiCLIProjectID(GeminiCLIProjectSelection{
		RequestedProjectID: "frontend-project",
		ResponseProjectID:  " backend-project ",
		TierID:             "FREE",
		ExplicitProject:    true,
	})
	if got != "backend-project" {
		t.Fatalf("project id = %q, want backend-project", got)
	}
}

func TestResolveGeminiCLIProjectIDPrefersBackendProjectForLegacyTier(t *testing.T) {
	got := ResolveGeminiCLIProjectID(GeminiCLIProjectSelection{
		RequestedProjectID: "frontend-project",
		ResponseProjectID:  "backend-project",
		TierID:             "LEGACY",
		ExplicitProject:    true,
	})
	if got != "backend-project" {
		t.Fatalf("project id = %q, want backend-project", got)
	}
}

func TestResolveGeminiCLIProjectIDPreservesPaidExplicitProject(t *testing.T) {
	got := ResolveGeminiCLIProjectID(GeminiCLIProjectSelection{
		RequestedProjectID: " paid-project ",
		ResponseProjectID:  "backend-project",
		TierID:             "PRO",
		ExplicitProject:    true,
	})
	if got != "paid-project" {
		t.Fatalf("project id = %q, want paid-project", got)
	}
}

func TestResolveGeminiCLIProjectIDUsesResponseForAutoDiscovery(t *testing.T) {
	got := ResolveGeminiCLIProjectID(GeminiCLIProjectSelection{
		RequestedProjectID: "auto-project",
		ResponseProjectID:  " backend-project ",
		TierID:             "PRO",
	})
	if got != "backend-project" {
		t.Fatalf("project id = %q, want backend-project", got)
	}
}

func TestResolveGeminiCLIProjectIDFallsBackToRequestedProject(t *testing.T) {
	got := ResolveGeminiCLIProjectID(GeminiCLIProjectSelection{
		RequestedProjectID: " requested-project ",
		ResponseProjectID:  " ",
		ExplicitProject:    true,
	})
	if got != "requested-project" {
		t.Fatalf("project id = %q, want requested-project", got)
	}
}

func TestGeminiCLIUserAgentUsesCurrentTerminalShape(t *testing.T) {
	got := GeminiCLIUserAgent("gemini-3-pro")
	if !strings.HasPrefix(got, "GeminiCLI/0.34.0/gemini-3-pro (") {
		t.Fatalf("user agent prefix = %q", got)
	}
	if !strings.HasSuffix(got, "; terminal)") {
		t.Fatalf("user agent suffix = %q", got)
	}
}
