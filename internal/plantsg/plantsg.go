// Package plantsg is a planted defect: the semgrep job must fail on it.
package plantsg

import (
	"context"
	"os/exec"
)

// Run hands a command string to a shell.
func Run(ctx context.Context, s string) error {
	return exec.CommandContext(ctx, "sh", "-c", s).Run()
}
