package main

import (
	"context"
	"flag"
	"time"

	"github.com/tillknuesting/fliessband/internal/pipeline"
	"github.com/tillknuesting/fliessband/internal/sandbox"
)

// sandboxFlag adds -sandbox to a command's flags.
func sandboxFlag(fs *flag.FlagSet) *string {
	return fs.String("sandbox", "", "where generated code runs: none or docker (default $FLIESSBAND_SANDBOX, else none)")
}

// setupSandbox configures how the manager executes generated code. An
// explicitly requested container sandbox that cannot be reached is an
// error, never a silent fallback to running code on the host.
func setupSandbox(m *pipeline.Manager, mode string) error {
	c, err := sandbox.FromEnv(mode, pipeline.DefaultSandboxImage())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.SetSandbox(ctx, c)
}
