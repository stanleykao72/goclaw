package agycli

import (
	"errors"
	"time"
)

// ErrEmptyPrompt is returned by BuildPrintArgs when PrintOptions.Prompt is empty.
var ErrEmptyPrompt = errors.New("agycli: prompt is required")

// PrintOptions is a typed view over the subset of agy's one-shot --print flags
// that the bridge drives. It exists to encode agy's argv contract in code and
// kill the argv-ordering bug we kept hitting by hand-assembling RunOptions.Args.
//
// agy's -p/--print is a string-valued flag (Go flag pkg): it consumes the NEXT
// token as its value, so the prompt MUST be -p's value, not a trailing
// positional. BuildPrintArgs always emits ["-p", Prompt, ...flags] so the prompt
// can never be misparsed as a flag (e.g. "--sandbox") and dropped.
type PrintOptions struct {
	Prompt          string        // required; becomes -p's value
	Model           string        // optional => --model <m>
	Sandbox         bool          // true => --sandbox
	SkipPermissions bool          // true => --dangerously-skip-permissions
	PrintTimeout    time.Duration // optional => --print-timeout <dur>
	AddDirs         []string      // optional => repeated --add-dir <d>
	Conversation    string        // optional => --conversation <id> (resume an agy-minted conversation)
	LogFile         string        // optional => --log-file <path> (per-run CLI log; used for first-turn conversation-ID capture)
}

// BuildPrintArgs returns the argv (everything after the binary) for a one-shot
// agy --print run, with the prompt encoded as -p's value.
//
// The result always starts with ["-p", o.Prompt] followed by the requested
// flags, in this order: --model, --sandbox, --dangerously-skip-permissions,
// --print-timeout, --conversation, --log-file, then one --add-dir <d> per
// AddDirs entry. Empty optional fields are omitted. Sandbox and SkipPermissions
// are independent: either, both, or neither may be set.
//
// It returns ErrEmptyPrompt (and a nil argv) when o.Prompt is empty, since an
// empty prompt would let the next flag slide into -p's value and silently drop
// the run's intent.
func BuildPrintArgs(o PrintOptions) ([]string, error) {
	if o.Prompt == "" {
		return nil, ErrEmptyPrompt
	}

	// Prompt is -p's value. Everything else is appended as ordinary flags.
	args := []string{"-p", o.Prompt}

	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Sandbox {
		args = append(args, "--sandbox")
	}
	if o.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if o.PrintTimeout > 0 {
		// agy parses Go-style durations (e.g. "5m0s"); time.Duration.String()
		// emits exactly that form.
		args = append(args, "--print-timeout", o.PrintTimeout.String())
	}
	if o.Conversation != "" {
		args = append(args, "--conversation", o.Conversation)
	}
	if o.LogFile != "" {
		args = append(args, "--log-file", o.LogFile)
	}
	for _, d := range o.AddDirs {
		if d == "" {
			continue
		}
		args = append(args, "--add-dir", d)
	}

	return args, nil
}
