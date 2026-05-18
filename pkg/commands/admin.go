package commands

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/yolocs/ocifactory/pkg/auth/backend"
	"github.com/yolocs/ocifactory/pkg/handler"
	adminhandler "github.com/yolocs/ocifactory/pkg/handler/admin"
	"github.com/yolocs/ocifactory/pkg/logging"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	flagNamespacePrefix = "namespace-prefix"
)

type adminServeConfig struct {
	Port            string `mapstructure:"port"`
	BackendRegistry string `mapstructure:"backend-registry"`
	RepoPrefix      string `mapstructure:"repo-prefix"`
	NamespacePrefix string `mapstructure:"namespace-prefix"`
	EnableMetrics   bool   `mapstructure:"enable-metrics"`
	MetricsPath     string `mapstructure:"metrics-path"`

	BackendAuthKind                 string   `mapstructure:"backend-auth-kind"`
	BackendAuthGCPADCScopes         []string `mapstructure:"backend-auth-gcpadc-scopes"`
	BackendAuthStaticEnvUserEnv     string   `mapstructure:"backend-auth-staticenv-user-env"`
	BackendAuthStaticEnvPasswordEnv string   `mapstructure:"backend-auth-staticenv-password-env"`
	BackendAuthDockerConfigPath     string   `mapstructure:"backend-auth-dockerconfig-path"`

	RegistryURL *url.URL `mapstructure:"-"`
}

func (c *adminServeConfig) Validate() error {
	var merr error
	if c.Port == "" {
		merr = errors.Join(merr, fmt.Errorf("port is required"))
	}
	if c.BackendRegistry == "" {
		merr = errors.Join(merr, fmt.Errorf("backend-registry is required"))
		return merr
	}
	if !strings.HasPrefix(c.BackendRegistry, "http://") && !strings.HasPrefix(c.BackendRegistry, "https://") {
		c.BackendRegistry = "https://" + c.BackendRegistry
	}
	u, err := url.Parse(c.BackendRegistry)
	if err != nil {
		merr = errors.Join(merr, fmt.Errorf("failed to parse backend-registry URL: %w", err))
	} else {
		c.RegistryURL = u
	}
	if err := oci.ValidateRepoPrefix(c.RepoPrefix); err != nil {
		merr = errors.Join(merr, err)
	}
	return merr
}

func (c *adminServeConfig) backendAuthConfig() backendAuthConfig {
	return backendAuthConfig{
		Kind:                 c.BackendAuthKind,
		GCPADCScopes:         c.BackendAuthGCPADCScopes,
		StaticEnvUserEnv:     c.BackendAuthStaticEnvUserEnv,
		StaticEnvPasswordEnv: c.BackendAuthStaticEnvPasswordEnv,
		DockerConfigPath:     c.BackendAuthDockerConfigPath,
	}
}

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Run ocifactory control-plane commands.",
	}
	cmd.AddCommand(newAdminServeCmd())
	return cmd
}

func newAdminServeCmd() *cobra.Command {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv(flagPort, "PORT")
	return buildAdminServeCmd(v)
}

func buildAdminServeCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control-plane admin HTTP service.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg adminServeConfig
			if err := v.Unmarshal(&cfg); err != nil {
				return fmt.Errorf("unmarshal config: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid flags: %w", err)
			}
			return runAdminServe(cmd.Context(), &cfg)
		},
	}
	registerAdminServeFlags(cmd.Flags())
	if err := v.BindPFlags(cmd.Flags()); err != nil {
		panic(fmt.Errorf("bind flags to viper: %w", err))
	}
	return cmd
}

func registerAdminServeFlags(flags *pflag.FlagSet) {
	flags.String(flagPort, "8081", "The port the admin server listens to.")
	flags.String(flagBackendRegistry, "", "The URL to the backend OCI registry.")
	flags.String(flagRepoPrefix, "", "Single-segment OCI repository prefix for this ocifactory instance.")
	flags.String(flagNamespacePrefix, namespace.DefaultPrefix,
		"OCI repository prefix for namespace metadata and the global namespace index.")
	flags.Bool(flagEnableMetrics, true,
		"Expose Prometheus metrics at --metrics-path and instrument HTTP and OCI backend layers.")
	flags.String(flagMetricsPath, "/metrics", "Path that serves Prometheus exposition when metrics are enabled.")

	flags.String(flagBackendAuthKind, backend.KindAnonymous,
		fmt.Sprintf("Credential provider for the backend OCI registry. Allowed: %v.", backend.AllKinds))
	flags.StringSlice(flagBackendAuthGCPADCScopes, nil, "Comma-separated OAuth2 scopes for the gcpadc backend auth kind. Empty = cloud-platform.")
	flags.String(flagBackendAuthStaticEnvUserEnv, "", "Name of the env var holding the username for the staticenv backend auth kind.")
	flags.String(flagBackendAuthStaticEnvPasswordEnv, "", "Name of the env var holding the password for the staticenv backend auth kind.")
	flags.String(flagBackendAuthDockerConfigPath, "", "Path to a docker-format config.json for the dockerconfig backend auth kind. Empty = ~/.docker/config.json.")
}

func runAdminServe(ctx context.Context, cfg *adminServeConfig) error {
	logging.NewFromEnv("OCIFACTORY_").WarnContext(ctx, "ADMIN SERVICE HAS NO INTERNAL AUTHENTICATION — deploy it behind platform/network access controls; see docs/admin.md")

	rec, metricsHandler := buildRecorder(cfg.EnableMetrics)
	bp, err := buildBackendAuth(ctx, cfg.backendAuthConfig())
	if err != nil {
		return fmt.Errorf("failed to build backend credentials: %w", err)
	}

	reg, err := oci.NewRegistry(cfg.RegistryURL,
		oci.WithArtifactType(namespace.ArtifactType),
		oci.WithMetrics(rec),
		oci.WithBackendAuth(bp),
		oci.WithRepoPrefix(cfg.RepoPrefix),
	)
	if err != nil {
		return fmt.Errorf("failed to create registry: %w", err)
	}
	store := namespace.NewStore(reg, namespace.WithPrefix(cfg.NamespacePrefix))
	// The admin service does not authorize data-plane requests, but
	// namespace.Registry already owns the package-index decoding logic
	// needed for soft-delete emptiness checks. Keep it here only as a
	// control-plane PackageLister.
	nsReg := namespace.NewRegistry(reg, store)
	ah, err := adminhandler.NewHandler(store, nsReg)
	if err != nil {
		return fmt.Errorf("failed to create admin handler: %w", err)
	}

	h := handler.WrapWithFormat(adminhandler.FormatLabel)(ah.Mux())
	metricsPath := cfg.MetricsPath
	if !cfg.EnableMetrics {
		metricsPath = ""
	}
	h = handler.ObservabilityHandler(h, handler.PingerFunc(func(ctx context.Context) error {
		_, err := store.List(ctx)
		return err
	}), metricsHandler, metricsPath)

	srv, err := handler.NewServer(cfg.Port, handler.Loggeer, handler.MetricsMiddleware(rec))
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}
	return srv.Start(ctx, h)
}
