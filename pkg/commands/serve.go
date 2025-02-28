package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/abcxyz/pkg/cli"
)

type serveFlags struct {
	port        string
	repoType    string
	registryURL string
	logLevel    string
}

func (f *serveFlags) Validate() error {
	var merr error
	if f.port == "" {
		merr = errors.Join(merr, fmt.Errorf("port is required"))
	}
	if f.repoType == "" {
		merr = errors.Join(merr, fmt.Errorf("repo-type is required"))
	}
	if f.registryURL == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required"))
	}
	if f.logLevel == "" {
		merr = errors.Join(merr, fmt.Errorf("loglevel is required"))
	}
	return merr
}

type ServeCommand struct {
	cli.BaseCommand

	flags *serveFlags
}

func (c *ServeCommand) Desc() string {
	return "Run the server to serve a specific artifact type."
}

func (c *ServeCommand) Help() string {
	return `
Usage: {{ COMMAND }} [options]
`
}

func (c *ServeCommand) Flags() *cli.FlagSet {
	set := c.NewFlagSet()
	sec := set.NewSection("OPTIONS")

	sec.StringVar(&cli.StringVar{
		Name:    "port",
		Target:  &c.flags.port,
		EnvVar:  "PORT",
		Default: "8080",
		Usage:   `The port the server listens to.`,
	})

	sec.StringVar(&cli.StringVar{
		Name:    "repo-type",
		Aliases: []string{"t"},
		Usage:   "Type of repository to serve. Allowed: [TBD]",
		EnvVar:  "OCIFACTORY_REPO_TYPE",
		Target:  &c.flags.repoType,
	})

	sec.StringVar(&cli.StringVar{
		Name:   "backend-registry",
		Usage:  "The URL to the backend OCI registry.",
		EnvVar: "OCIFACTORY_BACKEND_REGISTRY",
		Target: &c.flags.registryURL,
	})

	sec.StringVar(&cli.StringVar{
		Name:    "loglevel",
		Aliases: []string{"v"}, // Verbosity
		Usage:   "The log level.",
		EnvVar:  "OCIFACTORY_LOGLEVEL",
		Target:  &c.flags.logLevel,
		Default: "info",
	})

	return nil
}

func (c *ServeCommand) Run(ctx context.Context, args []string) error {
	f := c.Flags()
	if err := f.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}
	if err := c.flags.Validate(); err != nil {
		return fmt.Errorf("invalid flags: %w", err)
	}

	return fmt.Errorf("not implemented")
}
