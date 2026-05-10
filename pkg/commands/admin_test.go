package commands

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/yolocs/ocifactory/pkg/auth/backend"
)

func minimalAdminServeConfig() adminServeConfig {
	return adminServeConfig{
		Port:            "8081",
		BackendRegistry: "http://example.com",
		EnableMetrics:   true,
		MetricsPath:     "/metrics",
		BackendAuthKind: backend.KindAnonymous,
	}
}

func TestAdminServeConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mut     func(*adminServeConfig)
		wantErr string
		wantURL *url.URL
	}{
		{
			name:    "valid",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "default https scheme",
			mut: func(c *adminServeConfig) {
				c.BackendRegistry = "registry.example.com/ocifactory"
			},
			wantURL: &url.URL{Scheme: "https", Host: "registry.example.com", Path: "/ocifactory"},
		},
		{
			name: "missing port",
			mut: func(c *adminServeConfig) {
				c.Port = ""
			},
			wantErr: "port is required",
			wantURL: &url.URL{Scheme: "http", Host: "example.com"},
		},
		{
			name: "missing backend registry",
			mut: func(c *adminServeConfig) {
				c.BackendRegistry = ""
			},
			wantErr: "backend-registry is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalAdminServeConfig()
			if tc.mut != nil {
				tc.mut(&cfg)
			}
			err := cfg.Validate()
			if diff := cmp.Diff(tc.wantErr, errString(err)); diff != "" {
				t.Errorf("Validate() error mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantURL, cfg.RegistryURL, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Validate() RegistryURL mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAdminServeCmd_Flags(t *testing.T) {
	t.Parallel()

	cmd := newAdminServeCmd()
	tests := []string{
		flagPort,
		flagBackendRegistry,
		flagNamespacePrefix,
		flagEnableMetrics,
		flagMetricsPath,
		flagBackendAuthKind,
		flagBackendAuthGCPADCScopes,
		flagBackendAuthStaticEnvUserEnv,
		flagBackendAuthStaticEnvPasswordEnv,
		flagBackendAuthDockerConfigPath,
	}
	for _, flagName := range tests {
		t.Run(flagName, func(t *testing.T) {
			t.Parallel()
			if f := cmd.Flags().Lookup(flagName); f == nil {
				t.Fatalf("flag %q not registered", flagName)
			}
		})
	}
	notRegistered := []string{
		flagRepoType,
		flagDisableStreamingPush,
		flagAllowOverwrite,
		flagDisableBlobRedirect,
		flagSimpleIndexCacheTTL,
		flagPythonMaxUploadBytes,
	}
	for _, flagName := range notRegistered {
		if f := cmd.Flags().Lookup(flagName); f != nil {
			t.Fatalf("admin serve registered data-plane flag %q", flagName)
		}
	}
}

func TestAdminCmd_IsRegistered(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	adminCmd, _, err := root.Find([]string{"admin"})
	if err != nil {
		t.Fatalf("Find(admin): %v", err)
	}
	if diff := cmp.Diff("admin", adminCmd.Name()); diff != "" {
		t.Errorf("admin command name mismatch (-want +got):\n%s", diff)
	}
	serveCmd, _, err := root.Find([]string{"admin", "serve"})
	if err != nil {
		t.Fatalf("Find(admin serve): %v", err)
	}
	if diff := cmp.Diff("serve", serveCmd.Name()); diff != "" {
		t.Errorf("admin serve command name mismatch (-want +got):\n%s", diff)
	}
}

// TestAdminServeCmd_EnvVarBindings mutates process-global env, so cannot t.Parallel.
func TestAdminServeCmd_EnvVarBindings(t *testing.T) {
	t.Setenv("OCIFACTORY_BACKEND_REGISTRY", "zot.example.com:5000/ocifactory")
	t.Setenv("OCIFACTORY_NAMESPACE_PREFIX", "control-plane")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_KIND", "staticenv")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_STATICENV_USER_ENV", "REG_USER")
	t.Setenv("OCIFACTORY_BACKEND_AUTH_STATICENV_PASSWORD_ENV", "REG_PASS")
	t.Setenv("PORT", "9091")

	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv(flagPort, "PORT")

	cmd := buildAdminServeCmd(v)
	var got adminServeConfig
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return v.Unmarshal(&got)
	}

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := adminServeConfig{
		Port:                            "9091",
		BackendRegistry:                 "zot.example.com:5000/ocifactory",
		NamespacePrefix:                 "control-plane",
		EnableMetrics:                   true,
		MetricsPath:                     "/metrics",
		BackendAuthKind:                 "staticenv",
		BackendAuthStaticEnvUserEnv:     "REG_USER",
		BackendAuthStaticEnvPasswordEnv: "REG_PASS",
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
