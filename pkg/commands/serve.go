package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/auth/chain"
	// Importing oidc for the side effect of registering its
	// "oidc" kind with the auth registry. The package's exported
	// API isn't called from here; auth.Build resolves it through
	// the registry lookup.
	_ "github.com/yolocs/ocifactory/pkg/auth/oidc"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/handler/maven"
	"github.com/yolocs/ocifactory/pkg/handler/python"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/metrics"
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

	disableStreamingPush bool
	simpleIndexCacheTTL  time.Duration

	enableMetrics bool
	metricsPath   string

	authConfigPath string
	authNone       bool

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
		return merr
	}
	if f.authConfigPath != "" && f.authNone {
		merr = errors.Join(merr, fmt.Errorf("--auth-config and --disable-auth are mutually exclusive"))
	}
	if f.authConfigPath == "" && !f.authNone {
		merr = errors.Join(merr, fmt.Errorf("either --auth-config or --disable-auth must be set"))
	}
	// Default to https when the user omits the scheme. Either way, parse
	// unconditionally — the original code only assigned f.registryURL
	// inside the prepend branch, leaving registryURL nil when the user
	// passed an http:// or https:// URL directly.
	if !strings.HasPrefix(f.registryURLStr, "http://") && !strings.HasPrefix(f.registryURLStr, "https://") {
		f.registryURLStr = "https://" + f.registryURLStr
	}
	u, err := url.Parse(f.registryURLStr)
	if err != nil {
		merr = errors.Join(merr, fmt.Errorf("failed to parse backend-registry URL: %w", err))
	} else {
		f.registryURL = u
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
	cmd.Flags().BoolVar(&flags.disableStreamingPush, "disable-streaming-push", false,
		"Force every blob upload through the buffered + monolithic path. "+
			"Bodies above the in-memory threshold will spill to a temp file "+
			"instead of streaming via chunked PATCH. Set this only for "+
			"backend registries with broken or missing chunked-PATCH support.")
	cmd.Flags().DurationVar(&flags.simpleIndexCacheTTL, "simple-index-cache-ttl", python.DefaultSimpleIndexCacheTTL,
		"TTL for the per-replica per-package PyPI simple-index cache. "+
			"Within the TTL, repeated GET /simple/<pkg>/ requests skip the "+
			"backend ListFiles call. Successful uploads invalidate the entry "+
			"for the affected package. Set to 0 to disable caching. "+
			"Note: in multi-replica deployments, an upload landing on one "+
			"replica may take up to this long to be reflected by another.")
	cmd.Flags().BoolVar(&flags.enableMetrics, "enable-metrics", true,
		"Expose Prometheus metrics at --metrics-path and instrument the "+
			"HTTP and OCI backend layers. When false, the no-op recorder is "+
			"wired throughout and the metrics endpoint returns 404.")
	cmd.Flags().StringVar(&flags.metricsPath, "metrics-path", "/metrics",
		"Path on the main listener that serves Prometheus exposition. "+
			"Operators wanting authn / network ACLs on metrics should "+
			"front this with their own reverse proxy.")
	cmd.Flags().StringVar(&flags.authConfigPath, "auth-config", os.Getenv("OCIFACTORY_AUTH_CONFIG"),
		"Path to a YAML auth config file. Each entry instantiates an "+
			"authenticator and they are tried in the order listed. "+
			"Built-in kinds: oidc. See docs/auth.md for the schema "+
			"and for adding out-of-tree kinds. Mutually exclusive "+
			"with --disable-auth.")
	cmd.Flags().BoolVar(&flags.authNone, "disable-auth", false,
		"Disable authentication entirely. Wires AlwaysAnonymous and "+
			"logs a loud warning at startup. For local development "+
			"against unauthenticated zot only — never use in production.")

	return cmd
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func runServe(ctx context.Context, flags *serveFlags) error {
	rec, metricsHandler := buildRecorder(flags.enableMetrics)

	authn, err := buildAuthenticator(ctx, flags)
	if err != nil {
		return fmt.Errorf("failed to build authenticator: %w", err)
	}
	authMW := auth.Middleware(authn)

	var (
		h      http.Handler
		reg    *oci.Registry
		format string
	)
	registryOpts := []oci.RegistryOption{
		oci.WithStreamingPushDisabled(flags.disableStreamingPush),
		oci.WithMetrics(rec),
	}

	switch flags.repoType {
	case maven.RepoType:
		r, err := oci.NewRegistry(
			flags.registryURL,
			append(registryOpts, oci.WithArtifactType(maven.ArtifactType))...,
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		mh, err := maven.NewHandler(r, maven.WithAuthMiddleware(authMW))
		if err != nil {
			return fmt.Errorf("failed to create maven handler: %w", err)
		}
		reg, format, h = r, maven.RepoType, mh.Mux()
	case python.RepoType:
		r, err := oci.NewRegistry(
			flags.registryURL,
			append(registryOpts, oci.WithArtifactType(python.ArtifactType))...,
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		ph, err := python.NewHandler(r,
			python.WithSimpleIndexCacheTTL(flags.simpleIndexCacheTTL),
			python.WithAuthMiddleware(authMW),
		)
		if err != nil {
			return fmt.Errorf("failed to create python handler: %w", err)
		}
		reg, format, h = r, python.RepoType, ph.Mux()
	default:
		return fmt.Errorf("repo-type %q is not supported", flags.repoType)
	}

	// Wrap the format mux with the format tag, then with the
	// observability endpoints. MetricsMiddleware (registered below at
	// the server level) injects its state holder before either fires,
	// so SetFormat from inside WrapWithFormat propagates back up
	// through any gorilla/mux dispatch.
	h = handler.WrapWithFormat(format)(h)
	metricsPath := flags.metricsPath
	if !flags.enableMetrics {
		metricsPath = ""
	}
	h = handler.ObservabilityHandler(h, reg, metricsHandler, metricsPath)

	// Auth middleware is NOT installed at the server level —
	// each format handler chains it on its own router (or
	// sub-router). This leaves /healthz, /readyz, /metrics and
	// any future public-by-default routes ungated, and lets
	// formats like npm split public reads from authenticated
	// writes per route.
	srv, err := handler.NewServer(
		flags.port,
		handler.Loggeer,
		handler.MetricsMiddleware(rec),
	)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	return srv.Start(ctx, h)
}

// buildAuthenticator constructs the auth.Authenticator the server
// installs in front of every protected route. Two sources:
//
//  1. --disable-auth: AlwaysAnonymous, with a loud warning. Local
//     dev only.
//  2. --auth-config: load the YAML, instantiate one authenticator
//     per entry through the auth-kind registry, chain them in
//     declaration order.
//
// Validate() refuses to run with neither flag set, so any other
// branch is unreachable in production.
//
// The auth-kind registry (pkg/auth.RegisterKind) is what makes the
// surface pluggable: importing oidc registers "oidc"; an
// out-of-tree fork can side-effect-import its own kind.
func buildAuthenticator(ctx context.Context, flags *serveFlags) (auth.Authenticator, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	if flags.authNone {
		logger.WarnContext(ctx, "AUTHENTICATION DISABLED — every request authenticates as anonymous. "+
			"Do not use --disable-auth in production.")
		return auth.AlwaysAnonymous, nil
	}

	cfg, err := auth.LoadConfigFile(flags.authConfigPath)
	if err != nil {
		return nil, err
	}
	children, err := auth.Build(cfg)
	if err != nil {
		return nil, err
	}
	return chain.New(children...), nil
}

// buildRecorder returns the metrics recorder and the http.Handler that
// serves Prometheus exposition. When metrics are disabled both the
// recorder and the handler default to no-op equivalents so the rest of
// the wiring path can stay identical.
func buildRecorder(enabled bool) (metrics.Recorder, http.Handler) {
	if !enabled {
		return metrics.NoOp(), nil
	}
	p := metrics.NewPrometheus(nil)
	return p, p.Handler()
}
