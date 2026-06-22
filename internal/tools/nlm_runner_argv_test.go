package tools

import (
	"context"
	"reflect"
	"testing"
)

// TestExecNLMNotebookRunner_SourceSyncArgv asserts SourceSync shells the exact
// `nlm source sync <notebookId>` argv (notebook id is a SEPARATE element, never
// interpolated into a shell string).
func TestExecNLMNotebookRunner_SourceSyncArgv(t *testing.T) {
	var gotBinary string
	var gotArgs []string
	r := &execNLMNotebookRunner{
		binary: "nlm-test",
		run: func(ctx context.Context, binary string, args []string) ([]byte, error) {
			gotBinary = binary
			gotArgs = args
			return []byte(""), nil
		},
	}

	if err := r.SourceSync(context.Background(), "nb-123"); err != nil {
		t.Fatalf("SourceSync err = %v", err)
	}
	if gotBinary != "nlm-test" {
		t.Errorf("binary = %q, want nlm-test", gotBinary)
	}
	want := []string{"source", "sync", "nb-123"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("argv = %v, want %v", gotArgs, want)
	}
}
