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
	"github.com/yolocs/ocifactory/pkg/namespace"
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
// adding the OCIFACTORY_* env-var form). The matching mapstructure
// tags on serveConfig fields below MUST stay in sync — there's no
// compile-time check that the strings line up.
const (
	flagPort                            = "port"
	flagRepoType                        = "repo-type"
	flagBackendRegistry                 = "backend-registry"
	flagDisableStreamingPush            = "disable-streaming-push"
	flagDisableBlobRedirect             = "disable-blob-redirect"
	flagAllowOverwrite                  = "allow-overwrite"
	flagSimpleIndexCacheTTL             = "simple-index-cache-ttl"
	flagPythonMaxUploadBytes            = "python-max-upload-bytes"
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

// serveConfig is the typed view of the resolved CLI/env/default
// configuration. Populated by viper.Unmarshal, which uses viper's
// default DecodeHook to comma-split env-supplied slices and parse
// time.Duration strings. Validation runs against this struct;
// runServe and the build* helpers consume it directly.
type serveConfig struct {
	Port                 string        `mapstructure:"port"`
	RepoType             string        `mapstructure:"repo-type"`
	BackendRegistry      string        `mapstructure:"backend-registry"`
	DisableStreamingPush bool          `mapstructure:"disable-streaming-push"`
	DisableBlobRedirect  bool          `mapstructure:"disable-blob-redirect"`
	AllowOverwrite       bool          `mapstructure:"allow-overwrite"`
	SimpleIndexCacheTTL  time.Duration `mapstructure:"simple-index-cache-ttl"`
	PythonMaxUploadBytes int64         `mapstructure:"python-max-upload-bytes"`
	EnableMetrics        bool          `mapstructure:"enable-metrics"`
	MetricsPath          string        `mapstructure:"metrics-path"`

	DisableAuthn      bool     `mapstructure:"disable-authn"`
	AuthnKind         string   `mapstructure:"authn-kind"`
	AuthnOIDCIssuers  []string `mapstructure:"authn-oidc-issuers"`
	AuthnOIDCAudience string   `mapstructure:"authn-oidc-audience"`

	BackendAuthKind                 string   `mapstructure:"backend-auth-kind"`
	BackendAuthGCPADCScopes         []string `mapstructure:"backend-auth-gcpadc-scopes"`
	BackendAuthStaticEnvUserEnv     string   `mapstructure:"backend-auth-staticenv-user-env"`
	BackendAuthStaticEnvPasswordEnv string   `mapstructure:"backend-auth-staticenv-password-env"`
	BackendAuthDockerConfigPath     string   `mapstructure:"backend-auth-dockerconfig-path"`

	// RegistryURL is computed by Validate from BackendRegistry. Not
	// populated by Unmarshal — the `-` tag tells mapstructure to
	// skip it.
	RegistryURL *url.URL `mapstructure:"-"`
}

type backendAuthConfig struct {
	Kind                 string
	GCPADCScopes         []string
	StaticEnvUserEnv     string
	StaticEnvPasswordEnv string
	DockerConfigPath     string
}

func (c *serveConfig) backendAuthConfig() backendAuthConfig {
	return backendAuthConfig{
		Kind:                 c.BackendAuthKind,
		GCPADCScopes:         c.BackendAuthGCPADCScopes,
		StaticEnvUserEnv:     c.BackendAuthStaticEnvUserEnv,
		StaticEnvPasswordEnv: c.BackendAuthStaticEnvPasswordEnv,
		DockerConfigPath:     c.BackendAuthDockerConfigPath,
	}
}

// Validate checks that c is internally consistent and populates
// c.RegistryURL by parsing c.BackendRegistry. Returns the joined
// validation errors. Any URL the operator gave without an
// http(s):// scheme is rewritten to https:// in c itself.
func (c *serveConfig) Validate() error {
	var merr error
	if c.Port == "" {
		merr = errors.Join(merr, fmt.Errorf("port is required"))
	}
	repoSupported := false
	for _, t := range supportedRepoTypes {
		if t == c.RepoType {
			repoSupported = true
			break
		}
	}
	if !repoSupported {
		merr = errors.Join(merr, fmt.Errorf("repo-type %q is not supported", c.RepoType))
	}
	if repoTypesNeedingBackend[c.RepoType] && c.BackendRegistry == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required for --repo-type=%s", c.RepoType))
		return merr
	}
	if !c.DisableAuthn {
		if c.AuthnKind == "" {
			merr = errors.Join(merr, fmt.Errorf("either --authn-kind (OCIFACTORY_AUTHN_KIND) or --disable-authn must be set"))
		} else {
			supported := false
			for _, k := range supportedAuthnKinds {
				if k == c.AuthnKind {
					supported = true
					break
				}
			}
			if !supported {
				merr = errors.Join(merr, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", c.AuthnKind, supportedAuthnKinds))
			}
			if c.AuthnKind == authnKindOIDC {
				if len(c.AuthnOIDCIssuers) == 0 {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-issuers (OCIFACTORY_AUTHN_OIDC_ISSUERS) is required for --authn-kind=oidc"))
				}
				if c.AuthnOIDCAudience == "" {
					merr = errors.Join(merr, fmt.Errorf("--authn-oidc-audience (OCIFACTORY_AUTHN_OIDC_AUDIENCE) is required for --authn-kind=oidc"))
				}
			}
		}
	}
	if c.BackendRegistry != "" {
		if !strings.HasPrefix(c.BackendRegistry, "http://") && !strings.HasPrefix(c.BackendRegistry, "https://") {
			c.BackendRegistry = "https://" + c.BackendRegistry
		}
		u, err := url.Parse(c.BackendRegistry)
		if err != nil {
			merr = errors.Join(merr, fmt.Errorf("failed to parse backend-registry URL: %w", err))
		} else {
			c.RegistryURL = u
		}
	}
	return merr
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
// need. The viper instance is the layered config; Unmarshal in
// RunE converts it into a serveConfig that everything downstream
// consumes.
func buildServeCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server to serve a specific artifact type.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg serveConfig
			if err := v.Unmarshal(&cfg); err != nil {
				return fmt.Errorf("unmarshal config: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid flags: %w", err)
			}
			return runServe(cmd.Context(), &cfg)
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
// given pflag.FlagSet without any struct binding. pflag still sets
// the default each flag advertises in --help; viper holds the
// resolved values, and Unmarshal in RunE writes them into a
// serveConfig.
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
	flags.Bool(flagAllowOverwrite, false,
		"Allow re-uploading a file whose (repo, version, name) tuple "+
			"already exists. The default (false) rejects re-uploads with "+
			"409 Conflict, matching PyPI's auditability story and keeping "+
			"every published version immutable. Set this true for "+
			"workflows that intentionally re-publish under the same "+
			"version (Maven snapshots, fix-the-CI-job retries, ephemeral "+
			"staging) — overwrites unlink the previous file manifest from "+
			"the version's referrer set so readers always see exactly "+
			"one match per filename.")
	flags.Bool(flagDisableBlobRedirect, false,
		"Disable redirecting blob downloads to backend-issued presigned "+
			"URLs. By default, hosted backends (GAR, ECR, ACR, GHCR, Docker "+
			"Hub) get a 307 to their CDN/object-store URL on blob GET, "+
			"saving ocifactory egress on read-heavy workloads. Set this in "+
			"environments where exposing backend URLs to clients is "+
			"unacceptable (egress restrictions, DLP, audit requirements). "+
			"Self-hosted inline-serving backends (zot, Harbor, distribution) "+
			"are unaffected — they fall back to streaming automatically.")
	flags.Duration(flagSimpleIndexCacheTTL, python.DefaultSimpleIndexCacheTTL,
		"TTL for the per-replica per-package PyPI simple-index cache. "+
			"Within the TTL, repeated GET /simple/<pkg>/ requests skip the "+
			"backend ListFiles call. Successful uploads invalidate the entry "+
			"for the affected package. Set to 0 to disable caching. "+
			"Note: in multi-replica deployments, an upload landing on one "+
			"replica may take up to this long to be reflected by another.")
	flags.Int64(flagPythonMaxUploadBytes, python.DefaultMaxUploadBytes,
		"Cap on the total request-body size accepted by the python upload "+
			"endpoint. Defends against an authenticated client streaming "+
			"arbitrary bytes to burn instance hours / egress before the OCI "+
			"backend rejects the layer. Set to 0 to disable the cap.")
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

func runServe(ctx context.Context, cfg *serveConfig) error {
	rec, metricsHandler := buildRecorder(cfg.EnableMetrics)

	authn, err := buildAuthn(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to build authenticator: %w", err)
	}
	authMW := auth.Middleware(authn)

	bp, err := buildBackendAuth(ctx, cfg.backendAuthConfig())
	if err != nil {
		return fmt.Errorf("failed to build backend credentials: %w", err)
	}

	var (
		h      http.Handler
		reg    *oci.Registry
		format string
	)
	registryOpts := []oci.RegistryOption{
		oci.WithStreamingPushDisabled(cfg.DisableStreamingPush),
		oci.WithBlobRedirectDisabled(cfg.DisableBlobRedirect),
		oci.WithAllowOverwrite(cfg.AllowOverwrite),
		oci.WithMetrics(rec),
		oci.WithBackendAuth(bp),
	}

	switch cfg.RepoType {
	case maven.RepoType:
		r, err := oci.NewRegistry(
			cfg.RegistryURL,
			append(registryOpts, oci.WithArtifactType(maven.ArtifactType))...,
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		storeReg, err := newNamespaceMetadataRegistry(cfg.RegistryURL, registryOpts)
		if err != nil {
			return fmt.Errorf("failed to create namespace registry: %w", err)
		}
		nsReg := namespace.NewRegistry(r, namespace.NewStore(storeReg))
		mh, err := maven.NewHandler(nsReg, maven.WithAuthMiddleware(authMW))
		if err != nil {
			return fmt.Errorf("failed to create maven handler: %w", err)
		}
		reg, format, h = r, maven.RepoType, mh.Mux()
	case python.RepoType:
		r, err := oci.NewRegistry(
			cfg.RegistryURL,
			append(registryOpts, oci.WithArtifactType(python.ArtifactType))...,
		)
		if err != nil {
			return fmt.Errorf("failed to create registry: %w", err)
		}
		storeReg, err := newNamespaceMetadataRegistry(cfg.RegistryURL, registryOpts)
		if err != nil {
			return fmt.Errorf("failed to create namespace registry: %w", err)
		}
		nsReg := namespace.NewRegistry(r, namespace.NewStore(storeReg))
		ph, err := python.NewHandler(nsReg,
			python.WithSimpleIndexCacheTTL(cfg.SimpleIndexCacheTTL),
			python.WithMaxUploadBytes(cfg.PythonMaxUploadBytes),
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
		return fmt.Errorf("repo-type %q is not supported", cfg.RepoType)
	}

	// Wrap the format mux with the format tag, then with the
	// observability endpoints. MetricsMiddleware (registered below at
	// the server level) injects its state holder before either fires,
	// so SetFormat from inside WrapWithFormat propagates back up
	// through any gorilla/mux dispatch.
	h = handler.WrapWithFormat(format)(h)
	metricsPath := cfg.MetricsPath
	if !cfg.EnableMetrics {
		metricsPath = ""
	}
	h = handler.ObservabilityHandler(h, reg, metricsHandler, metricsPath)

	// Auth middleware is NOT installed at the server level — each
	// format handler chains it on its own router (or sub-router).
	// This leaves /healthz, /readyz, /metrics and any future
	// public-by-default routes ungated.
	srv, err := handler.NewServer(
		cfg.Port,
		handler.Loggeer,
		handler.MetricsMiddleware(rec),
	)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	return srv.Start(ctx, h)
}

func newNamespaceMetadataRegistry(registryURL *url.URL, baseOpts []oci.RegistryOption) (*oci.Registry, error) {
	opts := append([]oci.RegistryOption{}, baseOpts...)
	opts = append(opts, oci.WithArtifactType(namespace.ArtifactType))
	return oci.NewRegistry(registryURL, opts...)
}

// buildAuthn constructs the auth.Authenticator the server installs
// in front of every protected route. Two paths:
//
//  1. --disable-authn (OCIFACTORY_AUTHN_DISABLED=true): wires
//     AlwaysAnonymous with a loud warning. Local dev only.
//  2. --authn-kind=oidc: builds one OIDC authenticator per entry
//     in --authn-oidc-issuers, all sharing --authn-oidc-audience,
//     and chains them in declaration order so a token whose iss
//     matches any configured issuer is accepted.
//
// Validate refuses to run with neither set, so any other branch is
// unreachable in production.
func buildAuthn(ctx context.Context, cfg *serveConfig) (auth.Authenticator, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	if cfg.DisableAuthn {
		logger.WarnContext(ctx, "AUTHENTICATION DISABLED — every request authenticates as anonymous. "+
			"Do not use --disable-authn in production.")
		return auth.AlwaysAnonymous, nil
	}
	switch cfg.AuthnKind {
	case authnKindOIDC:
		children := make([]auth.Authenticator, 0, len(cfg.AuthnOIDCIssuers))
		for _, issuer := range cfg.AuthnOIDCIssuers {
			a, err := oidc.New(issuer, cfg.AuthnOIDCAudience)
			if err != nil {
				return nil, fmt.Errorf("oidc issuer %q: %w", issuer, err)
			}
			children = append(children, a)
		}
		return chain.New(children...), nil
	default:
		// Validate catches this earlier; defensive return so an
		// out-of-tree main calling runServe directly still gets a
		// clean error.
		return nil, fmt.Errorf("authn-kind %q is not supported (allowed: %v)", cfg.AuthnKind, supportedAuthnKinds)
	}
}

// buildBackendAuth constructs the credential provider ocifactory
// presents to the backend OCI registry. When the operator hasn't
// set --backend-auth-kind / OCIFACTORY_BACKEND_AUTH_KIND, the
// default is anonymous and a warning is logged so the implicit
// no-credential intent isn't silent.
func buildBackendAuth(ctx context.Context, cfg backendAuthConfig) (backend.Provider, error) {
	logger := logging.NewFromEnv("OCIFACTORY_")
	bcfg := backend.Config{
		Kind:                 cfg.Kind,
		GCPADCScopes:         cfg.GCPADCScopes,
		StaticEnvUserEnv:     cfg.StaticEnvUserEnv,
		StaticEnvPasswordEnv: cfg.StaticEnvPasswordEnv,
		DockerConfigPath:     cfg.DockerConfigPath,
	}
	if bcfg.Kind == "" || bcfg.Kind == backend.KindAnonymous {
		logger.WarnContext(ctx, "no backend credential provider configured (set --backend-auth-kind / "+
			"OCIFACTORY_BACKEND_AUTH_KIND). Falling back to anonymous backend access — only safe for "+
			"public read-only registries.")
	}
	return backend.New(bcfg)
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
