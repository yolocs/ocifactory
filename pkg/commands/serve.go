package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// Flag-name constants. Used as both the cobra flag name and the
// viper key (with the env-prefix and dash→underscore mapping
// adding the OCIFACTORY_* env-var form). Centralised so the
// validation, runServe, and test paths share one source of truth.
const (
	flagPort                            = "port"
	flagRepoType                        = "repo-type"
	flagBackendRegistry                 = "backend-registry"
	flagDisableStreamingPush            = "disable-streaming-push"
	flagSimpleIndexCacheTTL             = "simple-index-cache-ttl"
	flagEnableMetrics                   = "enable-metrics"
	flagMetricsPath                     = "metrics-path"
	flagDisableAuthn                    = "disable-authn"
	flagAuthnKind                       = "authn-kind"
	flagAuthnOIDCIssuers                = "authn-oidc-issuers"
	flagAuthnOIDCAudience               = "authn-oidc-audience"
	flagBackendAuthKind                 = "backend-auth-kind"
	flagBackendAuthGCPADCScopes         = "backend-auth-gcpadc-scopes"
	flagBackendAuthStaticEnvUserEnv     = "backend-auth-staticenv-user-env"
	flagBackendAuthStaticEnvPasswordEnv = "backend-auth-staticenv-password-env"
	flagBackendAuthDockerConfigPath     = "backend-auth-dockerconfig-path"
)

// validateServeConfig checks that v's resolved values (CLI > env >
// default, per viper's documented precedence) are internally
// consistent. Returns the parsed backend-registry URL — nil when
// the operator didn't supply one — so runServe doesn't have to
// re-parse it.
//
// Any URL the operator gave without an http(s):// scheme is
// rewritten to https:// in v itself, so a downstream Get of
// backend-registry sees the canonical form.
func validateServeConfig(v *viper.Viper) (*url.URL, error) {
	var merr error
	if v.GetString(flagPort) == "" {
		merr = errors.Join(merr, fmt.Errorf("port is required"))
	}
	repoType := v.GetString(flagRepoType)
	repoSupported := false
	for _, t := range supportedRepoTypes {
		if t == repoType {
			repoSupported = true
			break
		}
	}
	if !repoSupported {
		merr = errors.Join(merr, fmt.Errorf("repo-type %q is not supported", repoType))
	}

	registryURLStr := v.GetString(flagBackendRegistry)
	if repoTypesNeedingBackend[repoType] && registryURLStr == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required for --repo-type=%s", repoType))
		return nil, merr
	}

	if !v.GetBool(flagDisableAuthn) {
		kind := v.GetString(flagAuthnKind)
		if kind == "" {
			merr = errors.Join(merr, fmt.Errorf("either --authn-kind (OCIFACTORY_AUTHN_KIND) or --disable-authn must be set"))
		} else {
			supported := false
			for _, k := range supportedAuthnKinds {
				if k == kind {
					supported = true
					break
				}
			}
			if !supported {
				merr = errors.Join(merr, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", kind, supportedAuthnKinds))
			}
			if kind == authnKindOIDC {
				if len(stringSliceCSV(v, flagAuthnOIDCIssuers)) == 0 {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-issuers (OCIFACTORY_AUTHN_OIDC_ISSUERS) is required for --authn-kind=oidc"))
				}
				if v.GetString(flagAuthnOIDCAudience) == "" {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-audience (OCIFACTORY_AUTHN_OIDC_AUDIENCE) is required for --authn-kind=oidc"))
				}
			}
		}
	}

	var registryURL *url.URL
	if registryURLStr != "" {
		// Default to https when the user omits the scheme.
		if !strings.HasPrefix(registryURLStr, "http://") && !strings.HasPrefix(registryURLStr, "https://") {
			registryURLStr = "https://" + registryURLStr
			v.Set(flagBackendRegistry, registryURLStr)
		}
		u, err := url.Parse(registryURLStr)
		if err != nil {
			merr = errors.Join(merr, fmt.Errorf("failed to parse backend-registry URL: %w", err))
		} else {
			registryURL = u
		}
	}
	return registryURL, merr
}

// stringSliceCSV reads a string slice from v while accommodating
// viper's known gap: GetStringSlice doesn't comma-split env-supplied
// values. pflag's StringSlice handles the CLI form (returning a
// real []string), and SetDefault preserves slice defaults verbatim;
// only the env path needs explicit splitting, detected by a
// single-element result whose element contains a comma.
func stringSliceCSV(v *viper.Viper, key string) []string {
	raw := v.GetStringSlice(key)
	if len(raw) == 1 && strings.Contains(raw[0], ",") {
		parts := strings.Split(raw[0], ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return raw
}

func newServeCmd() *cobra.Command {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	// PORT is the PaaS convention (Cloud Run, Heroku, …) and lives
	// outside the OCIFACTORY_* namespace; bind it explicitly so
	// viper picks it up.
	_ = v.BindEnv(flagPort, "PORT")
	return buildServeCmd(v)
}

// buildServeCmd is the shared constructor — newServeCmd builds the
// production viper instance, tests pass in one wired the way they
// need. The viper instance becomes the single source of truth for
// every value RunE / runServe / validateServeConfig consumes.
func buildServeCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server to serve a specific artifact type.",
		RunE: func(cmd *cobra.Command, args []string) error {
			registryURL, err := validateServeConfig(v)
			if err != nil {
				return fmt.Errorf("invalid flags: %w", err)
			}
			return runServe(cmd.Context(), v, registryURL)
		},
	}

	registerServeFlags(cmd.Flags())
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		// BindPFlags only fails on a nil flag set, which is
		// impossible — registerServeFlags just defined every flag.
		panic(fmt.Errorf("bind flags to viper: %w", err))
	}
	return cmd
}

// registerServeFlags installs the serve command's CLI flags on the
// given pflag.FlagSet without binding them to a struct: viper holds
// every value, and runServe / validateServeConfig read directly
// from viper. pflag still sets the default each flag advertises in
// --help, which is what viper's "pflag default" precedence layer
// returns when no override exists.
func registerServeFlags(flags *pflag.FlagSet) {
	flags.String(flagPort, "8080",
		"The port the server listens to.")
	flags.StringP(flagRepoType, "t", "",
		fmt.Sprintf("Type of repository to serve. Allowed: %v", supportedRepoTypes))
	flags.String(flagBackendRegistry, "",
		"The URL to the backend OCI registry.")
	flags.Bool(flagDisableStreamingPush, false,
		"Force every blob upload through the buffered + monolithic path. "+
			"Bodies above the in-memory threshold will spill to a temp file "+
			"instead of streaming via chunked PATCH. Set this only for "+
			"backend registries with broken or missing chunked-PATCH support.")
	flags.Duration(flagSimpleIndexCacheTTL, python.DefaultSimpleIndexCacheTTL,
		"TTL for the per-replica per-package PyPI simple-index cache. "+
			"Within the TTL, repeated GET /simple/<pkg>/ requests skip the "+
			"backend ListFiles call. Successful uploads invalidate the entry "+
			"for the affected package. Set to 0 to disable caching. "+
			"Note: in multi-replica deployments, an upload landing on one "+
			"replica may take up to this long to be reflected by another.")
	flags.Bool(flagEnableMetrics, true,
		"Expose Prometheus metrics at --metrics-path and instrument the "+
			"HTTP and OCI backend layers. When false, the no-op recorder is "+
			"wired throughout and the metrics endpoint returns 404.")
	flags.String(flagMetricsPath, "/metrics",
		"Path on the main listener that serves Prometheus exposition. "+
			"Operators wanting authn / network ACLs on metrics should "+
			"front this with their own reverse proxy.")

	// Frontend authentication.
	flags.Bool(flagDisableAuthn, false,
		"Disable inbound authentication entirely. Wires AlwaysAnonymous "+
			"and logs a loud warning at startup. For local development "+
			"against unauthenticated zot only — never use in production. "+
			"Mutually exclusive with --authn-kind.")
	flags.String(flagAuthnKind, "",
		fmt.Sprintf("Authenticator kind. Allowed: %v.", supportedAuthnKinds))
	flags.StringSlice(flagAuthnOIDCIssuers, nil,
		"Comma-separated list of trusted OIDC issuer URLs. One authenticator "+
			"is built per issuer and they are tried in declaration order — a "+
			"token whose iss matches any entry is accepted. Required when "+
			"--authn-kind=oidc.")
	flags.String(flagAuthnOIDCAudience, "",
		"Required audience claim every accepted OIDC token must carry. "+
			"Required when --authn-kind=oidc.")

	// Backend OCI registry credentials.
	flags.String(flagBackendAuthKind, backend.KindAnonymous,
		fmt.Sprintf("Credential provider for the backend OCI registry. "+
			"Allowed: %v. Defaults to %q (empty credential, fine for public "+
			"read-only registries; fails closed against private ones).",
			backend.AllKinds, backend.KindAnonymous))
	flags.StringSlice(flagBackendAuthGCPADCScopes, nil,
		"Comma-separated OAuth2 scopes for the gcpadc backend auth kind. "+
			"Empty = cloud-platform.")
	flags.String(flagBackendAuthStaticEnvUserEnv, "",
		"Name of the env var holding the username for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	flags.String(flagBackendAuthStaticEnvPasswordEnv, "",
		"Name of the env var holding the password for the staticenv backend "+
			"auth kind. Required when --backend-auth-kind=staticenv.")
	flags.String(flagBackendAuthDockerConfigPath, "",
		"Path to a docker-format config.json for the dockerconfig backend "+
			"auth kind. Empty = ~/.docker/config.json.")
}

func runServe(ctx context.Context, v *viper.Viper, registryURL *url.URL) error {
	rec, metricsHandler := buildRecorder(v.GetBool(flagEnableMetrics))

	authn, err := buildAuthn(ctx, v)
	if err != nil {
		return fmt.Errorf("failed to build authenticator: %w", err)
	}
	authMW := auth.Middleware(authn)

	bp, err := buildBackendAuth(ctx, v)
	if err != nil {
		return fmt.Errorf("failed to build backend credentials: %w", err)
	}

	var (
		h      http.Handler
		reg    *oci.Registry
		format string
	)
	registryOpts := []oci.RegistryOption{
		oci.WithStreamingPushDisabled(v.GetBool(flagDisableStreamingPush)),
		oci.WithMetrics(rec),
		oci.WithBackendAuth(bp),
	}

	repoType := v.GetString(flagRepoType)
	switch repoType {
	case maven.RepoType:
		r, err := oci.NewRegistry(
			registryURL,
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
			registryURL,
			append(registryOpts, oci.WithArtifactType(python.ArtifactType))...,
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		ph, err := python.NewHandler(r,
			python.WithSimpleIndexCacheTTL(v.GetDuration(flagSimpleIndexCacheTTL)),
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
		return fmt.Errorf("repo-type %q is not supported", repoType)
	}

	// Wrap the format mux with the format tag, then with the
	// observability endpoints. MetricsMiddleware (registered below at
	// the server level) injects its state holder before either fires,
	// so SetFormat from inside WrapWithFormat propagates back up
	// through any gorilla/mux dispatch.
	h = handler.WrapWithFormat(format)(h)
	metricsPath := v.GetString(flagMetricsPath)
	if !v.GetBool(flagEnableMetrics) {
		metricsPath = ""
	}
	h = handler.ObservabilityHandler(h, reg, metricsHandler, metricsPath)

	// Auth middleware is NOT installed at the server level — each
	// format handler chains it on its own router (or sub-router).
	// This leaves /healthz, /readyz, /metrics and any future
	// public-by-default routes ungated.
	srv, err := handler.NewServer(
		v.GetString(flagPort),
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
// validateServeConfig refuses to run with neither set, so any
// other branch is unreachable in production.
func buildAuthn(ctx context.Context, v *viper.Viper) (auth.Authenticator, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	if v.GetBool(flagDisableAuthn) {
		logger.WarnContext(ctx, "AUTHENTICATION DISABLED — every request authenticates as anonymous. "+
			"Do not use --disable-authn in production.")
		return auth.AlwaysAnonymous, nil
	}
	kind := v.GetString(flagAuthnKind)
	switch kind {
	case authnKindOIDC:
		issuers := stringSliceCSV(v, flagAuthnOIDCIssuers)
		audience := v.GetString(flagAuthnOIDCAudience)
		children := make([]auth.Authenticator, 0, len(issuers))
		for _, issuer := range issuers {
			a, err := oidc.New(issuer, audience)
			if err != nil {
				return nil, fmt.Errorf("oidc issuer %q: %w", issuer, err)
			}
			children = append(children, a)
		}
		return chain.New(children...), nil
	default:
		// validateServeConfig catches this earlier; defensive
		// return so an out-of-tree main calling runServe directly
		// still gets a clean error.
		return nil, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", kind, supportedAuthnKinds)
	}
}

// buildBackendAuth constructs the credential provider ocifactory
// presents to the backend OCI registry, from the
// OCIFACTORY_BACKEND_AUTH_* configuration. When the operator hasn't
// set --backend-auth-kind / OCIFACTORY_BACKEND_AUTH_KIND, the
// default is anonymous and a warning is logged so the implicit
// no-credential intent isn't silent.
func buildBackendAuth(ctx context.Context, v *viper.Viper) (backend.Provider, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	cfg := backend.Config{
		Kind:                 v.GetString(flagBackendAuthKind),
		GCPADCScopes:         stringSliceCSV(v, flagBackendAuthGCPADCScopes),
		StaticEnvUserEnv:     v.GetString(flagBackendAuthStaticEnvUserEnv),
		StaticEnvPasswordEnv: v.GetString(flagBackendAuthStaticEnvPasswordEnv),
		DockerConfigPath:     v.GetString(flagBackendAuthDockerConfigPath),
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
