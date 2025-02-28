package commands

import (
	"context"

	"github.com/abcxyz/pkg/cli"
)

var rootCmd = func() cli.Command {
	return &cli.RootCommand{
		Name:    "ocifactory",
		Version: "dev",
		Commands: map[string]cli.CommandFactory{
			"serve": func() cli.Command { return &ServeCommand{} },
		},
	}
}

// Run executes the CLI.
func Run(ctx context.Context, args []string) error {
	return rootCmd().Run(ctx, args) //nolint:wrapcheck // Want passthrough
}
