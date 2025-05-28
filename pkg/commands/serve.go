package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/abcxyz/pkg/cli"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/handler/maven"
	"github.com/yolocs/ocifactory/pkg/handler/npm" // Added import
	"github.com/yolocs/ocifactory/pkg/handler/python"
	"github.com/yolocs/ocifactory/pkg/oci"
)

var (
	supportedRepoTypes = []string{
		maven.RepoType,
		python.RepoType,
		npm.RepoType, // Added npm.RepoType
	}
)

type serveFlags struct {
	port           string
	repoType       string
	registryURLStr string
	landingDir     string

	registryURL *url.URL
}

func (f *serveFlags) Validate() error {
	var merr error
	if f.port == "" {
		merr = errors.Join(merr, fmt.Errorf("port is required"))
	}
	repoSupported := false
	for _, repoType := range supportedRepoTypes {
		if repoType == f.repoType {
			repoSupported = true
			break
		}
	}
	if !repoSupported {
		merr = errors.Join(merr, fmt.Errorf("repo-type %q is not supported", f.repoType))
	}
	if f.registryURLStr == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required"))
	}
	if !strings.HasPrefix(f.registryURLStr, "http://") && !strings.HasPrefix(f.registryURLStr, "https://") {
		// Default to https.
		f.registryURLStr = "https://" + f.registryURLStr
		u, err := url.Parse(f.registryURLStr)
		if err != nil {
			merr = errors.Join(merr, fmt.Errorf("failed to parse backend-registry URL: %w", err))
		} else {
			f.registryURL = u
		}
	}
	// This default is implicit because temp dir will be different each time.
	if f.landingDir == "" {
		f.landingDir = os.TempDir()
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
	c.flags = &serveFlags{}
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
		Usage:   fmt.Sprintf("Type of repository to serve. Allowed: [%s]", strings.Join(supportedRepoTypes, ", ")), // Updated usage string
		EnvVar:  "OCIFACTORY_REPO_TYPE",
		Target:  &c.flags.repoType,
	})

	sec.StringVar(&cli.StringVar{
		Name:   "backend-registry",
		Usage:  "The URL to the backend OCI registry.",
		EnvVar: "OCIFACTORY_BACKEND_REGISTRY",
		Target: &c.flags.registryURLStr,
	})

	sec.StringVar(&cli.StringVar{
		Name:   "landing-dir",
		Usage:  "The directory to store the temporary artifact files. If not set, a temp dir will be created each time.",
		EnvVar: "OCIFACTORY_LANDING_DIR",
		Target: &c.flags.landingDir,
	})

	return set
}

func (c *ServeCommand) Run(ctx context.Context, args []string) error {
	f := c.Flags()
	if err := f.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}
	if err := c.flags.Validate(); err != nil {
		return fmt.Errorf("invalid flags: %w", err)
	}

	var h http.Handler
	switch c.flags.repoType {
	case maven.RepoType:
		reg, err := oci.NewRegistry(
			c.flags.registryURL,
			oci.WithLandingDir(c.flags.landingDir),
			oci.WithArtifactType(maven.ArtifactType),
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		mh, err := maven.NewHandler(reg)
		if err != nil {
			return fmt.Errorf("failed to create maven handler: %w", err)
		}
		h = mh.Mux()
	case python.RepoType:
		reg, err := oci.NewRegistry(
			c.flags.registryURL,
			oci.WithLandingDir(c.flags.landingDir),
			oci.WithArtifactType(python.ArtifactType),
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		ph, err := python.NewHandler(reg)
		if err != nil {
			return fmt.Errorf("failed to create python handler: %w", err)
		}
		h = ph.Mux()
	case npm.RepoType: // Added case for npm.RepoType
		reg, err := oci.NewRegistry(
			c.flags.registryURL,
			oci.WithLandingDir(c.flags.landingDir),
			oci.WithArtifactType(npm.ArtifactType), // Using npm.ArtifactType as defined
		)
		if err != nil {
			return fmt.Errorf("failed to create registry for npm: %w", err)
		}
		npmHandler, err := npm.NewHandler(reg)
		if err != nil {
			return fmt.Errorf("failed to create npm handler: %w", err)
		}
		h = npmHandler.Mux()
	default:
		return fmt.Errorf("repo-type %q is not supported", c.flags.repoType)
	}

	srv, err := handler.NewServer(c.flags.port, handler.PassThroughAuth, handler.Loggeer)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	return srv.Start(ctx, h)
}
