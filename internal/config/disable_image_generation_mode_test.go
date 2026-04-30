package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisableImageGenerationMode_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want DisableImageGenerationMode
	}{
		{name: "false", raw: "disable-image-generation: false\n", want: DisableImageGenerationOff},
		{name: "true", raw: "disable-image-generation: true\n", want: DisableImageGenerationAll},
		{name: "chat", raw: "disable-image-generation: chat\n", want: DisableImageGenerationChat},
		{name: "quoted true", raw: "disable-image-generation: \"true\"\n", want: DisableImageGenerationAll},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Value DisableImageGenerationMode `yaml:"disable-image-generation"`
			}
			if err := yaml.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", err)
			}
			if got.Value != tt.want {
				t.Fatalf("mode = %v, want %v", got.Value, tt.want)
			}
		})
	}
}

func TestDisableImageGenerationMode_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want DisableImageGenerationMode
	}{
		{name: "false", raw: "false", want: DisableImageGenerationOff},
		{name: "true", raw: "true", want: DisableImageGenerationAll},
		{name: "chat", raw: `"chat"`, want: DisableImageGenerationChat},
		{name: "null", raw: "null", want: DisableImageGenerationOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got DisableImageGenerationMode
			if err := json.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("mode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDisableImageGenerationMode_Marshal(t *testing.T) {
	tests := []struct {
		name     string
		mode     DisableImageGenerationMode
		wantJSON string
		wantYAML any
	}{
		{name: "false", mode: DisableImageGenerationOff, wantJSON: "false", wantYAML: false},
		{name: "true", mode: DisableImageGenerationAll, wantJSON: "true", wantYAML: true},
		{name: "chat", mode: DisableImageGenerationChat, wantJSON: `"chat"`, wantYAML: "chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawJSON, err := json.Marshal(tt.mode)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if string(rawJSON) != tt.wantJSON {
				t.Fatalf("json = %s, want %s", rawJSON, tt.wantJSON)
			}
			rawYAML, err := tt.mode.MarshalYAML()
			if err != nil {
				t.Fatalf("MarshalYAML() error = %v", err)
			}
			if rawYAML != tt.wantYAML {
				t.Fatalf("yaml = %#v, want %#v", rawYAML, tt.wantYAML)
			}
		})
	}
}

func TestDisableImageGenerationMode_InvalidValue(t *testing.T) {
	var got DisableImageGenerationMode
	if err := json.Unmarshal([]byte(`"images"`), &got); err == nil {
		t.Fatalf("json.Unmarshal() error = nil, want invalid value error")
	}
	var wrapper struct {
		Value DisableImageGenerationMode `yaml:"disable-image-generation"`
	}
	if err := yaml.Unmarshal([]byte("disable-image-generation: images\n"), &wrapper); err == nil {
		t.Fatalf("yaml.Unmarshal() error = nil, want invalid value error")
	}
}
