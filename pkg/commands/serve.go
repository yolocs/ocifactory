package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/handler/maven"
	"github.com/yolocs/ocifactory/pkg/handler/python"
	"github.com/yolocs/ocifactory/pkg/oci"
)

var supportedRepoTypes = []string{
	maven.RepoType,
	python.RepoType,
}

type serveFlags struct {
	port           string
	repoType       string
	registryURLStr string

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
	return merr
}

func newServeCmd() *cobra.Command {
	flags := &serveFlags{}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server to serve a specific artifact type.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.Validate(); err != nil {
				return fmt.Errorf("invalid flags: %w", err)
			}
			return runServe(cmd.Context(), flags)
		},
	}

	cmd.Flags().StringVar(&flags.port, "port", envOr("PORT", "8080"),
		"The port the server listens to.")
	cmd.Flags().StringVarP(&flags.repoType, "repo-type", "t", os.Getenv("OCIFACTORY_REPO_TYPE"),
		fmt.Sprintf("Type of repository to serve. Allowed: %v", supportedRepoTypes))
	cmd.Flags().StringVar(&flags.registryURLStr, "backend-registry", os.Getenv("OCIFACTORY_BACKEND_REGISTRY"),
		"The URL to the backend OCI registry.")

	return cmd
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func runServe(ctx context.Context, flags *serveFlags) error {
	var h http.Handler
	switch flags.repoType {
	case maven.RepoType:
		reg, err := oci.NewRegistry(
			flags.registryURL,
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
			flags.registryURL,
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
	default:
		return fmt.Errorf("repo-type %q is not supported", flags.repoType)
	}

	srv, err := handler.NewServer(flags.port, handler.PassThroughAuth, handler.Loggeer)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	return srv.Start(ctx, h)
}
