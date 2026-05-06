package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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

// envPrefix is the namespace every OCIFACTORY_* env var sits under.
// Combined with the dash→underscore key replacer, viper maps each
// flag-name to its env var automatically: --authn-oidc-issuers ↔
// OCIFACTORY_AUTHN_OIDC_ISSUERS, --backend-auth-kind ↔
// OCIFACTORY_BACKEND_AUTH_KIND, etc.
const envPrefix = "OCIFACTORY"

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

	// Per-command viper instance so each test (and future
	// sub-commands) get isolated state. AutomaticEnv + the
	// dash→underscore replacer turns every flag-name into the
	// matching OCIFACTORY_* env var, so adding a new flag never
	// needs a separate env-var lookup line.
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	// PORT is the PaaS convention (Cloud Run, Heroku, …) and lives
	// outside the OCIFACTORY_* namespace; bind it explicitly so
	// viper picks it up.
	_ = v.BindEnv("port", "PORT")

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server to serve a specific artifact type.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := loadServeFlags(v, cmd, flags); err != nil {
				return err
			}
			if err := flags.Validate(); err != nil {
				return fmt.Errorf("invalid flags: %w", err)
			}
			return runServe(cmd.Context(), flags)
		},
	}

	cmd.Flags().StringVar(&flags.port, "port", "8080",
		"The port the server listens to.")
	cmd.Flags().StringVarP(&flags.repoType, "repo-type", "t", "",
		fmt.Sprintf("Type of repository to serve. Allowed: %v", supportedRepoTypes))
	cmd.Flags().StringVar(&flags.registryURLStr, "backend-registry", "",
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
	cmd.Flags().BoolVar(&flags.authnDisabled, "disable-authn", false,
		"Disable inbound authentication entirely. Wires AlwaysAnonymous "+
			"and logs a loud warning at startup. For local development "+
			"against unauthenticated zot only — never use in production. "+
			"Mutually exclusive with --authn-kind.")
	cmd.Flags().StringVar(&flags.authnKind, "authn-kind", "",
		fmt.Sprintf("Authenticator kind. Allowed: %v.", supportedAuthnKinds))
	cmd.Flags().StringSliceVar(&flags.authnOIDCIssuers, "authn-oidc-issuers", nil,
		"Comma-separated list of trusted OIDC issuer URLs. One authenticator "+
			"is built per issuer and they are tried in declaration order — a "+
			"token whose iss matches any entry is accepted. Required when "+
			"--authn-kind=oidc.")
	cmd.Flags().StringVar(&flags.authnOIDCAudience, "authn-oidc-audience", "",
		"Required audience claim every accepted OIDC token must carry. "+
			"Required when --authn-kind=oidc.")

	// Backend OCI registry credentials.
	cmd.Flags().StringVar(&flags.backendAuthKind, "backend-auth-kind", backend.KindAnonymous,
		fmt.Sprintf("Credential provider for the backend OCI registry. "+
			"Allowed: %v. Defaults to %q (empty credential, fine for public "+
			"read-only registries; fails closed against private ones).",
			backend.AllKinds, backend.KindAnonymous))
	cmd.Flags().StringSliceVar(&flags.backendAuthGCPADCScopes, "backend-auth-gcpadc-scopes", nil,
		"Comma-separated OAuth2 scopes for the gcpadc backend auth kind. "+
			"Empty = cloud-platform.")
	cmd.Flags().StringVar(&flags.backendAuthStaticEnvUserEnv, "backend-auth-staticenv-user-env", "",
		"Name of the env var holding the username for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	cmd.Flags().StringVar(&flags.backendAuthStaticEnvPasswordEnv, "backend-auth-staticenv-password-env", "",
		"Name of the env var holding the password for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	cmd.Flags().StringVar(&flags.backendAuthDockerConfigPath, "backend-auth-dockerconfig-path", "",
		"Path to a docker-format config.json for the dockerconfig backend "+
			"auth kind. Empty = ~/.docker/config.json.")

	// Bind every flag to viper so an env-only deployment works. The
	// only error path here is "flag set is nil", which is impossible
	// — newServeCmd just defined every flag above.
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		panic(fmt.Errorf("bind flags to viper: %w", err))
	}

	return cmd
}

// loadServeFlags pulls the post-precedence (CLI flag > env var >
// default) values out of viper and writes them back into the
// serveFlags struct. Necessary because cobra's flag parser already
// populated the struct with the defaults; viper holds the final
// values once env vars are layered in.
//
// Slice flags go through the bound pflag value rather than
// viper.GetStringSlice so a comma-separated env var (e.g.
// OCIFACTORY_AUTHN_OIDC_ISSUERS=a,b,c) is parsed by pflag's
// StringSlice machinery into ["a","b","c"] — viper's own decoder
// returns ["a,b,c"] for env-supplied slices, which is the wrong
// shape.
func loadServeFlags(v *viper.Viper, cmd *cobra.Command, f *serveFlags) error {
	f.port = v.GetString("port")
	f.repoType = v.GetString("repo-type")
	f.registryURLStr = v.GetString("backend-registry")
	f.disableStreamingPush = v.GetBool("disable-streaming-push")
	f.simpleIndexCacheTTL = v.GetDuration("simple-index-cache-ttl")
	f.enableMetrics = v.GetBool("enable-metrics")
	f.metricsPath = v.GetString("metrics-path")

	f.authnDisabled = v.GetBool("disable-authn")
	f.authnKind = v.GetString("authn-kind")
	f.authnOIDCAudience = v.GetString("authn-oidc-audience")

	f.backendAuthKind = v.GetString("backend-auth-kind")
	f.backendAuthStaticEnvUserEnv = v.GetString("backend-auth-staticenv-user-env")
	f.backendAuthStaticEnvPasswordEnv = v.GetString("backend-auth-staticenv-password-env")
	f.backendAuthDockerConfigPath = v.GetString("backend-auth-dockerconfig-path")

	var err error
	if f.authnOIDCIssuers, err = readStringSlice(cmd, v, "authn-oidc-issuers"); err != nil {
		return err
	}
	if f.backendAuthGCPADCScopes, err = readStringSlice(cmd, v, "backend-auth-gcpadc-scopes"); err != nil {
		return err
	}
	return nil
}

// readStringSlice resolves a string-slice value following the
// CLI > env > default precedence, parsing comma-separated env vars
// through pflag's StringSlice (which is what handles quoting and
// trimming for the CLI form too).
func readStringSlice(cmd *cobra.Command, v *viper.Viper, name string) ([]string, error) {
	// CLI wins outright — viper.IsSet is true for explicit pflag set
	// even when the same value would also come from env.
	flag := cmd.Flags().Lookup(name)
	if flag != nil && flag.Changed {
		return cmd.Flags().GetStringSlice(name)
	}
	raw := v.GetString(name)
	if raw == "" {
		// Fall back to the flag's default (typically nil), preserving
		// the cobra-set value rather than returning an empty slice.
		if flag != nil {
			return cmd.Flags().GetStringSlice(name)
		}
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
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
