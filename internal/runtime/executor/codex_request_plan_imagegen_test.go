package executor

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexPrepareRequestPlan_InjectsImageGenerationToolForExplicitImageIntent(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","tool_choice":{"type":"image_generation"}}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.4",
		},
	}

	for _, mode := range []codexPreparedRequestPlanMode{
		codexPreparedRequestPlanExecute,
		codexPreparedRequestPlanExecuteStream,
		codexPreparedRequestPlanCompact,
	} {
		plan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, mode)
		if err != nil {
			t.Fatalf("prepareCodexRequestPlan(%s) error = %v", mode, err)
		}
		tools := gjson.GetBytes(plan.body, "tools").Array()
		if len(tools) != 1 {
			t.Fatalf("prepareCodexRequestPlan(%s) tools len = %d, want 1", mode, len(tools))
		}
		if got := tools[0].Get("type").String(); got != "image_generation" {
			t.Fatalf("prepareCodexRequestPlan(%s) tool type = %q, want %q", mode, got, "image_generation")
		}
	}
}

func TestCodexPrepareRequestPlan_DoesNotInjectImageGenerationToolForPlainTextRequest(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.4",
		},
	}

	plan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("prepareCodexRequestPlan() error = %v", err)
	}
	if got := gjson.GetBytes(plan.body, "tools"); got.Exists() {
		t.Fatalf("plain text tools = %s, want absent image_generation injection", got.Raw)
	}
}

func TestCodexPrepareRequestPlan_DoesNotDuplicateImageGenerationTool(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"input":"hello",
			"tools":[
				{"type":"function","name":"demo_tool"},
				{"type":"image_generation","output_format":"png"}
			]
		}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.4",
		},
	}

	plan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("prepareCodexRequestPlan() error = %v", err)
	}
	tools := gjson.GetBytes(plan.body, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools len = %d, want 2", len(tools))
	}
}

func TestCodexPrepareRequestPlan_SkipsImageGenerationToolForSpark(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex-spark",
		Payload: []byte(`{"model":"gpt-5.3-codex-spark","input":"hello","tool_choice":{"type":"image_generation"}}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.3-codex-spark",
		},
	}

	plan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("prepareCodexRequestPlan() error = %v", err)
	}
	if got := gjson.GetBytes(plan.body, "tools"); got.Exists() {
		t.Fatalf("spark tools = %s, want absent image_generation injection", got.Raw)
	}
}
