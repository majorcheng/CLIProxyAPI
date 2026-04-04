package cmd

import "testing"

func TestNormalizeVertexImportPrefix(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty", input: "", want: ""},
		{name: "trim surrounding slash", input: "/team-a/", want: "team-a"},
		{name: "trim spaces", input: "  team-a  ", want: "team-a"},
		{name: "reject nested slash", input: "team/a", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeVertexImportPrefix(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeVertexImportPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestVertexImportFileName(t *testing.T) {
	if got := vertexImportFileName("", "my-project"); got != "vertex-my-project.json" {
		t.Fatalf("unexpected file name without prefix: %s", got)
	}
	if got := vertexImportFileName("team-a", "my-project"); got != "vertex-team-a-my-project.json" {
		t.Fatalf("unexpected file name with prefix: %s", got)
	}
}
