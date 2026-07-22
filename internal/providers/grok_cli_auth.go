package providers

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
)

// GrokAuthStatus holds the parsed result of a Grok CLI login check.
// Grok has no `auth status` subcommand; `grok models` prints the login line
// ("You are logged in with <account>.") on its first line and exits 0.
type GrokAuthStatus struct {
	LoggedIn bool   `json:"loggedIn"`
	Account  string `json:"account,omitempty"`
}

// grokLoginLine matches "You are logged in with grok.com." capturing the
// account ("grok.com"); the lazy group + optional trailing period keep the
// sentence-final "." out of the capture.
var grokLoginLine = regexp.MustCompile(`(?i)logged in with\s+(\S+?)\.?(?:\s|$)`)

// CheckGrokAuthStatus runs `grok models` using the given CLI path and reports
// whether the local Grok subscription is authenticated. A non-zero exit or the
// absence of the login line is treated as not-logged-in rather than an error,
// so the endpoint can distinguish "binary missing" from "signed out".
func CheckGrokAuthStatus(ctx context.Context, cliPath string) (*GrokAuthStatus, error) {
	if cliPath == "" {
		cliPath = "grok"
	}

	resolvedPath, err := exec.LookPath(cliPath)
	if err != nil {
		return nil, fmt.Errorf("grok CLI binary not found at %q: %w", cliPath, err)
	}

	cmd := exec.CommandContext(ctx, resolvedPath, "models")
	output, _ := cmd.CombinedOutput() // signed-out may exit non-zero; parse output regardless

	if m := grokLoginLine.FindSubmatch(output); m != nil {
		return &GrokAuthStatus{LoggedIn: true, Account: string(m[1])}, nil
	}
	return &GrokAuthStatus{LoggedIn: false}, nil
}
