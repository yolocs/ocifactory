package commands

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/yolocs/ocifactory/internal/version"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "ocifactory",
		Short:         "ocifactory is a multi-format artifact registry backed by OCI.",
		Version:       version.HumanVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newServeCmd(), newAdminCmd())
	return cmd
}

// Run executes the CLI.
func Run(ctx context.Context, args []string) error {
	cmd := newRootCmd()
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx) //nolint:wrapcheck // Want passthrough
}
