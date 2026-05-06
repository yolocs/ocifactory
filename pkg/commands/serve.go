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
	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/auth/chain"
	"github.com/yolocs/ocifactory/pkg/auth/oidc"
	"github.com/yolocs/ocifactory/pkg/handler"
	"github.com/yolocs/ocifactory/pkg/handler/echo"
	"github.com/yolocs/ocifactory/pkg/handler/maven"
	"github.com/yolocs/ocifactory/pkg/handler/python"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/metrics"
	"github.com/yolocs/ocifactory/pkg/oci"
)

var supportedRepoTypes = []string{
	maven.RepoType,
	python.RepoType,
	echo.RepoType,
}

// repoTypesNeedingBackend lists the repo types that talk to an OCI
// backend. Anything not in this set runs without --backend-registry —
// echo is the only such type today and exists purely as an auth
// target for CI.
var repoTypesNeedingBackend = map[string]bool{
	maven.RepoType:  true,
	python.RepoType: true,
}

// Authenticator kind constants.
const (
	authnKindOIDC = "oidc"
)

var supportedAuthnKinds = []string{authnKindOIDC}

type serveFlags struct {
	port           string
	repoType       string
	registryURLStr string

	disableStreamingPush bool
	simpleIndexCacheTTL  time.Duration

	enableMetrics bool
	metricsPath   string

	// Frontend authentication. Configured via OCIFACTORY_AUTHN_*
	// env vars and the matching --authn-* flags. See buildAuthn.
	authnDisabled     bool
	authnKind         string
	authnOIDCIssuers  []string
	authnOIDCAudience string

	// Backend OCI registry credentials. Configured via
	// OCIFACTORY_BACKEND_AUTH_* env vars and the matching
	// --backend-auth-* flags. See buildBackendAuth.
	backendAuthKind                 string
	backendAuthGCPADCScopes         []string
	backendAuthStaticEnvUserEnv     string
	backendAuthStaticEnvPasswordEnv string
	backendAuthDockerConfigPath     string

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
	needsBackend := repoTypesNeedingBackend[f.repoType]
	if needsBackend && f.registryURLStr == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required for --repo-type=%s", f.repoType))
		return merr
	}
	if !f.authnDisabled {
		if f.authnKind == "" {
			merr = errors.Join(merr, fmt.Errorf("either --authn-kind (OCIFACTORY_AUTHN_KIND) or --disable-authn must be set"))
		} else {
			supported := false
			for _, k := range supportedAuthnKinds {
				if k == f.authnKind {
					supported = true
					break
				}
			}
			if !supported {
				merr = errors.Join(merr, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", f.authnKind, supportedAuthnKinds))
			}
			if f.authnKind == authnKindOIDC {
				if len(f.authnOIDCIssuers) == 0 {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-issuers (OCIFACTORY_AUTHN_OIDC_ISSUERS) is required for --authn-kind=oidc"))
				}
				if f.authnOIDCAudience == "" {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-audience (OCIFACTORY_AUTHN_OIDC_AUDIENCE) is required for --authn-kind=oidc"))
				}
			}
		}
	}
	if f.registryURLStr != "" {
		// Default to https when the user omits the scheme. Either way,
		// parse unconditionally — the original code only assigned
		// f.registryURL inside the prepend branch, leaving registryURL
		// nil when the user passed an http:// or https:// URL directly.
		if !strings.HasPrefix(f.registryURLStr, "http://") && !strings.HasPrefix(f.registryURLStr, "https://") {
			f.registryURLStr = "https://" + f.registryURLStr
		}
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

	// Frontend authentication flags. Single kind today (oidc); the
	// flag exists so adding mTLS / GitHub PAT / static passwords later
	// fits without an env-var-shape change.
	cmd.Flags().BoolVar(&flags.authnDisabled, "disable-authn", envBool("OCIFACTORY_AUTHN_DISABLED"),
		"Disable inbound authentication entirely. Wires AlwaysAnonymous "+
			"and logs a loud warning at startup. For local development "+
			"against unauthenticated zot only — never use in production. "+
			"Mutually exclusive with --authn-kind.")
	cmd.Flags().StringVar(&flags.authnKind, "authn-kind", os.Getenv("OCIFACTORY_AUTHN_KIND"),
		fmt.Sprintf("Authenticator kind. Allowed: %v.", supportedAuthnKinds))
	cmd.Flags().StringSliceVar(&flags.authnOIDCIssuers, "authn-oidc-issuers", envCSV("OCIFACTORY_AUTHN_OIDC_ISSUERS"),
		"Comma-separated list of trusted OIDC issuer URLs. One authenticator "+
			"is built per issuer and they are tried in declaration order — a "+
			"token whose iss matches any entry is accepted. Required when "+
			"--authn-kind=oidc.")
	cmd.Flags().StringVar(&flags.authnOIDCAudience, "authn-oidc-audience", os.Getenv("OCIFACTORY_AUTHN_OIDC_AUDIENCE"),
		"Required audience claim every accepted OIDC token must carry. "+
			"Required when --authn-kind=oidc.")

	// Backend OCI registry credentials.
	cmd.Flags().StringVar(&flags.backendAuthKind, "backend-auth-kind", envOr("OCIFACTORY_BACKEND_AUTH_KIND", backend.KindAnonymous),
		fmt.Sprintf("Credential provider for the backend OCI registry. "+
			"Allowed: %v. Defaults to %q (empty credential, fine for public "+
			"read-only registries; fails closed against private ones).",
			backend.AllKinds, backend.KindAnonymous))
	cmd.Flags().StringSliceVar(&flags.backendAuthGCPADCScopes, "backend-auth-gcpadc-scopes",
		envCSV("OCIFACTORY_BACKEND_AUTH_GCPADC_SCOPES"),
		"Comma-separated OAuth2 scopes for the gcpadc backend auth kind. "+
			"Empty = cloud-platform.")
	cmd.Flags().StringVar(&flags.backendAuthStaticEnvUserEnv, "backend-auth-staticenv-user-env",
		os.Getenv("OCIFACTORY_BACKEND_AUTH_STATICENV_USER_ENV"),
		"Name of the env var holding the username for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	cmd.Flags().StringVar(&flags.backendAuthStaticEnvPasswordEnv, "backend-auth-staticenv-password-env",
		os.Getenv("OCIFACTORY_BACKEND_AUTH_STATICENV_PASSWORD_ENV"),
		"Name of the env var holding the password for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	cmd.Flags().StringVar(&flags.backendAuthDockerConfigPath, "backend-auth-dockerconfig-path",
		os.Getenv("OCIFACTORY_BACKEND_AUTH_DOCKERCONFIG_PATH"),
		"Path to a docker-format config.json for the dockerconfig backend "+
			"auth kind. Empty = ~/.docker/config.json.")

	return cmd
}

// envOr returns the value of the named env var, or fallback when
// it's unset.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// envBool returns true iff the named env var is set to a truthy
// value. Anything not in the truthy set (including empty / unset)
// returns false.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// envCSV returns the named env var split on commas, with empty
// entries dropped and surrounding whitespace trimmed. Returns nil
// when the var is unset so cobra's StringSliceVar default-value
// semantics are preserved.
func envCSV(key string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func runServe(ctx context.Context, flags *serveFlags) error {
	rec, metricsHandler := buildRecorder(flags.enableMetrics)

	authn, err := buildAuthn(ctx, flags)
	if err != nil {
		return fmt.Errorf("failed to build authenticator: %w", err)
	}
	authMW := auth.Middleware(authn)

	bp, err := buildBackendAuth(ctx, flags)
	if err != nil {
		return fmt.Errorf("failed to build backend credentials: %w", err)
	}

	var (
		h      http.Handler
		reg    *oci.Registry
		format string
	)
	registryOpts := []oci.RegistryOption{
		oci.WithStreamingPushDisabled(flags.disableStreamingPush),
		oci.WithMetrics(rec),
		oci.WithBackendAuth(bp),
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
	case echo.RepoType:
		// echo is a no-op test format that does not talk to an OCI
		// backend, so reg stays nil — ObservabilityHandler treats a
		// nil pinger as "no backend configured" and /readyz collapses
		// to liveness, which is what we want.
		eh := echo.NewHandler(echo.WithAuthMiddleware(authMW))
		format, h = echo.RepoType, eh.Mux()
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

	// Auth middleware is NOT installed at the server level — each
	// format handler chains it on its own router (or sub-router).
	// This leaves /healthz, /readyz, /metrics and any future
	// public-by-default routes ungated.
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

// buildAuthn constructs the auth.Authenticator the server installs
// in front of every protected route, from the OCIFACTORY_AUTHN_*
// configuration. Two paths:
//
//  1. --disable-authn (OCIFACTORY_AUTHN_DISABLED=true): wires
//     AlwaysAnonymous with a loud warning. Local dev only.
//  2. --authn-kind=oidc: builds one OIDC authenticator per entry
//     in --authn-oidc-issuers, all sharing --authn-oidc-audience,
//     and chains them in declaration order so a token whose iss
//     matches any configured issuer is accepted.
//
// Validate() refuses to run with neither set, so any other branch
// is unreachable in production.
func buildAuthn(ctx context.Context, flags *serveFlags) (auth.Authenticator, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	if flags.authnDisabled {
		logger.WarnContext(ctx, "AUTHENTICATION DISABLED — every request authenticates as anonymous. "+
			"Do not use --disable-authn in production.")
		return auth.AlwaysAnonymous, nil
	}
	switch flags.authnKind {
	case authnKindOIDC:
		children := make([]auth.Authenticator, 0, len(flags.authnOIDCIssuers))
		for _, issuer := range flags.authnOIDCIssuers {
			a, err := oidc.New(issuer, flags.authnOIDCAudience)
			if err != nil {
				return nil, fmt.Errorf("oidc issuer %q: %w", issuer, err)
			}
			children = append(children, a)
		}
		return chain.New(children...), nil
	default:
		// Validate() catches this earlier; defensive return so an
		// out-of-tree main calling runServe directly still gets a
		// clean error.
		return nil, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", flags.authnKind, supportedAuthnKinds)
	}
}

// buildBackendAuth constructs the credential provider ocifactory
// presents to the backend OCI registry, from the
// OCIFACTORY_BACKEND_AUTH_* configuration. When the operator hasn't
// set --backend-auth-kind / OCIFACTORY_BACKEND_AUTH_KIND, the
// default is anonymous and a warning is logged so the implicit
// no-credential intent isn't silent.
func buildBackendAuth(ctx context.Context, flags *serveFlags) (backend.Provider, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	cfg := backend.Config{
		Kind:                 flags.backendAuthKind,
		GCPADCScopes:         flags.backendAuthGCPADCScopes,
		StaticEnvUserEnv:     flags.backendAuthStaticEnvUserEnv,
		StaticEnvPasswordEnv: flags.backendAuthStaticEnvPasswordEnv,
		DockerConfigPath:     flags.backendAuthDockerConfigPath,
	}
	if cfg.Kind == "" || cfg.Kind == backend.KindAnonymous {
		logger.WarnContext(ctx, "no backend credential provider configured (set --backend-auth-kind / "+
			"OCIFACTORY_BACKEND_AUTH_KIND). Falling back to anonymous backend access — only safe for "+
			"public read-only registries.")
	}
	return backend.New(cfg)
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
