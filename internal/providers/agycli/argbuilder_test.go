package agycli

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestBuildPrintArgs(t *testing.T) {
	tests := []struct {
		name    string
		opts    PrintOptions
		want    []string
		wantErr error
	}{
		{
			name:    "empty prompt errors",
			opts:    PrintOptions{},
			wantErr: ErrEmptyPrompt,
		},
		{
			name:    "empty prompt with other fields still errors",
			opts:    PrintOptions{Model: "gemini-3-pro", Sandbox: true},
			wantErr: ErrEmptyPrompt,
		},
		{
			name: "prompt only - prompt is -p's value at positions 0,1",
			opts: PrintOptions{Prompt: "hello world"},
			want: []string{"-p", "hello world"},
		},
		{
			name: "prompt that looks like a flag stays as -p's value",
			opts: PrintOptions{Prompt: "--dangerously-skip-permissions"},
			want: []string{"-p", "--dangerously-skip-permissions"},
		},
		{
			name: "model on",
			opts: PrintOptions{Prompt: "hi", Model: "gemini-3-pro"},
			want: []string{"-p", "hi", "--model", "gemini-3-pro"},
		},
		{
			name: "model off (empty) omitted",
			opts: PrintOptions{Prompt: "hi", Model: ""},
			want: []string{"-p", "hi"},
		},
		{
			name: "sandbox on",
			opts: PrintOptions{Prompt: "hi", Sandbox: true},
			want: []string{"-p", "hi", "--sandbox"},
		},
		{
			name: "sandbox off omitted",
			opts: PrintOptions{Prompt: "hi", Sandbox: false},
			want: []string{"-p", "hi"},
		},
		{
			name: "skip permissions on",
			opts: PrintOptions{Prompt: "hi", SkipPermissions: true},
			want: []string{"-p", "hi", "--dangerously-skip-permissions"},
		},
		{
			name: "skip permissions off omitted",
			opts: PrintOptions{Prompt: "hi", SkipPermissions: false},
			want: []string{"-p", "hi"},
		},
		{
			name: "sandbox and skip permissions are independent - both on",
			opts: PrintOptions{Prompt: "hi", Sandbox: true, SkipPermissions: true},
			want: []string{"-p", "hi", "--sandbox", "--dangerously-skip-permissions"},
		},
		{
			name: "sandbox on, skip permissions off",
			opts: PrintOptions{Prompt: "hi", Sandbox: true, SkipPermissions: false},
			want: []string{"-p", "hi", "--sandbox"},
		},
		{
			name: "sandbox off, skip permissions on",
			opts: PrintOptions{Prompt: "hi", Sandbox: false, SkipPermissions: true},
			want: []string{"-p", "hi", "--dangerously-skip-permissions"},
		},
		{
			name: "print timeout set uses duration String form",
			opts: PrintOptions{Prompt: "hi", PrintTimeout: 5 * time.Minute},
			want: []string{"-p", "hi", "--print-timeout", "5m0s"},
		},
		{
			name: "print timeout zero omitted",
			opts: PrintOptions{Prompt: "hi", PrintTimeout: 0},
			want: []string{"-p", "hi"},
		},
		{
			name: "print timeout sub-minute",
			opts: PrintOptions{Prompt: "hi", PrintTimeout: 90 * time.Second},
			want: []string{"-p", "hi", "--print-timeout", "1m30s"},
		},
		{
			name: "single add dir",
			opts: PrintOptions{Prompt: "hi", AddDirs: []string{"/tmp/a"}},
			want: []string{"-p", "hi", "--add-dir", "/tmp/a"},
		},
		{
			name: "multiple add dirs repeated",
			opts: PrintOptions{Prompt: "hi", AddDirs: []string{"/tmp/a", "/tmp/b", "/tmp/c"}},
			want: []string{"-p", "hi", "--add-dir", "/tmp/a", "--add-dir", "/tmp/b", "--add-dir", "/tmp/c"},
		},
		{
			name: "empty add dir entries skipped",
			opts: PrintOptions{Prompt: "hi", AddDirs: []string{"/tmp/a", "", "/tmp/b"}},
			want: []string{"-p", "hi", "--add-dir", "/tmp/a", "--add-dir", "/tmp/b"},
		},
		{
			name: "conversation set",
			opts: PrintOptions{Prompt: "hi", Conversation: "1ea82809-3482-44b6-ac01-4abdf0bd7fa2"},
			want: []string{"-p", "hi", "--conversation", "1ea82809-3482-44b6-ac01-4abdf0bd7fa2"},
		},
		{
			name: "conversation empty omitted",
			opts: PrintOptions{Prompt: "hi", Conversation: ""},
			want: []string{"-p", "hi"},
		},
		{
			name: "log file set",
			opts: PrintOptions{Prompt: "hi", LogFile: "/tmp/run.log"},
			want: []string{"-p", "hi", "--log-file", "/tmp/run.log"},
		},
		{
			name: "log file empty omitted",
			opts: PrintOptions{Prompt: "hi", LogFile: ""},
			want: []string{"-p", "hi"},
		},
		{
			name: "all flags together in canonical order",
			opts: PrintOptions{
				Prompt:          "do the thing",
				Model:           "gemini-3-pro",
				Sandbox:         true,
				SkipPermissions: true,
				PrintTimeout:    2 * time.Minute,
				AddDirs:         []string{"/work/one", "/work/two"},
				Conversation:    "5cb5ca2f-753a-48f9-81d2-c6c301d4bd86",
				LogFile:         "/tmp/agy-run.log",
			},
			want: []string{
				"-p", "do the thing",
				"--model", "gemini-3-pro",
				"--sandbox",
				"--dangerously-skip-permissions",
				"--print-timeout", "2m0s",
				"--conversation", "5cb5ca2f-753a-48f9-81d2-c6c301d4bd86",
				"--log-file", "/tmp/agy-run.log",
				"--add-dir", "/work/one",
				"--add-dir", "/work/two",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildPrintArgs(tt.opts)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("BuildPrintArgs() error = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("BuildPrintArgs() argv = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildPrintArgs() unexpected error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildPrintArgs() argv =\n  %#v\nwant\n  %#v", got, tt.want)
			}
		})
	}
}

// TestBuildPrintArgs_PromptValuePosition guards the core contract: the prompt is
// always token index 1 (the value of -p at index 0), never a trailing positional
// that agy's string-valued -p flag could swallow a sibling flag in place of.
func TestBuildPrintArgs_PromptValuePosition(t *testing.T) {
	got, err := BuildPrintArgs(PrintOptions{
		Prompt:          "PONG",
		SkipPermissions: true,
		Model:           "gemini-3-pro",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) < 2 || got[0] != "-p" || got[1] != "PONG" {
		t.Fatalf("prompt not at -p value position: %#v", got)
	}
}
