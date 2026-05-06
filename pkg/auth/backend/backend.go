// Package backend defines the pluggable provider interface used to
// authenticate ocifactory's calls to the backend OCI registry.
//
// Provider is intentionally a small, typed interface owned by this
// package. The previous design exposed oras-go's auth.CredentialFunc
// directly; that leaked an external dependency through ocifactory's
// public API and offered no place to evolve the credential shape
// without breaking consumers. Provider keeps the swap point ours.
//
// Built-in adapters (Anonymous, plus gcpadc / staticenv / dockerconfig
// under separate symbols in this package) are configured via the
// Config struct, which is itself populated from CLI flags / env vars
// in pkg/commands. Out-of-tree consumers implement Provider directly
// and pass an instance through pkg/oci's WithBackendAuth option.
package backend

import (
	"context"
	"errors"
	"fmt"
	"os"

	orascreds "oras.land/oras-go/v2/registry/remote/credentials"
)

// Credential is the credential a Provider returns. The shape mirrors
// what oras-go's auth client ultimately needs but is owned by this
// package so the upstream type isn't part of ocifactory's public API.
//
// Either a Username/Password pair OR an AccessToken is set; setting
// neither is the empty credential, which is what Anonymous returns.
type Credential struct {
	Username    string
	Password    string
	AccessToken string
}

// Provider supplies a credential for one call to the backend OCI
// registry. host is the registry host the call is targeting (e.g.
// "us-docker.pkg.dev"); providers that don't care about it (gcpadc,
// staticenv) ignore the argument.
//
// Returning a zero Credential is legal — it surfaces to the backend
// as an unauthenticated call, which is what public read-only
// registries accept. Providers should NOT return an error for "I
// have no credential for this host"; that's the empty Credential's
// job.
type Provider interface {
	Credential(ctx context.Context, host string) (Credential, error)
}

// ProviderFunc adapts a plain function to Provider. Useful for tests
// and one-off providers that don't carry state.
type ProviderFunc func(ctx context.Context, host string) (Credential, error)

// Credential calls f.
func (f ProviderFunc) Credential(ctx context.Context, host string) (Credential, error) {
	return f(ctx, host)
}

// Anonymous returns a Provider that always returns the empty
// Credential. Fine for public read-only backends; fails closed
// against any private backend.
func Anonymous() Provider {
	return ProviderFunc(func(_ context.Context, _ string) (Credential, error) {
		return Credential{}, nil
	})
}

// Kind constants enumerate every in-tree provider. Operators select
// one via OCIFACTORY_BACKEND_AUTH_KIND / --backend-auth-kind.
const (
	KindAnonymous    = "anonymous"
	KindGCPADC       = "gcpadc"
	KindStaticEnv    = "staticenv"
	KindDockerConfig = "dockerconfig"
)

// AllKinds lists every Kind New understands, in stable order. Used
// for help text and error messages so an operator with a typo sees
// what's actually available.
var AllKinds = []string{
	KindAnonymous,
	KindGCPADC,
	KindStaticEnv,
	KindDockerConfig,
}

// Config selects the in-tree Provider New constructs. Each Kind
// reads only its own sub-fields; the others are ignored. Populated
// in pkg/commands from the OCIFACTORY_BACKEND_AUTH_* env vars and
// the matching --backend-auth-* flags.
type Config struct {
	// Kind is the Kind* constant identifying which provider to
	// build. Empty defaults to KindAnonymous.
	Kind string

	// GCPADCScopes overrides the default OAuth2 scope for the
	// gcpadc provider. Empty falls back to cloud-platform.
	GCPADCScopes []string

	// StaticEnvUserEnv / StaticEnvPasswordEnv name the env vars
	// the staticenv provider reads on every call. Both required
	// when Kind == KindStaticEnv.
	StaticEnvUserEnv     string
	StaticEnvPasswordEnv string

	// DockerConfigPath overrides ~/.docker/config.json for the
	// dockerconfig provider. Empty falls back to the standard
	// docker location.
	DockerConfigPath string
}

// New constructs the in-tree Provider selected by cfg.Kind.
//
// Operators with bespoke credential needs (Vault, IAM Roles
// Anywhere, ...) bypass New entirely: they implement Provider
// themselves and pass it through oci.WithBackendAuth.
func New(cfg Config) (Provider, error) {
	kind := cfg.Kind
	if kind == "" {
		kind = KindAnonymous
	}
	switch kind {
	case KindAnonymous:
		return Anonymous(), nil
	case KindGCPADC:
		return newGCPADC(gcpadcOptions{Scopes: cfg.GCPADCScopes})
	case KindStaticEnv:
		return newStaticEnv(cfg.StaticEnvUserEnv, cfg.StaticEnvPasswordEnv)
	case KindDockerConfig:
		return newDockerConfig(cfg.DockerConfigPath)
	default:
		return nil, fmt.Errorf("backend: unknown kind %q (allowed: %v)", kind, AllKinds)
	}
}

// staticEnvProvider reads username/password from named env vars on
// every call so operators can rotate secrets via Vault Agent / SOPS
// / External Secrets without restarting. The cost is a pair of
// os.Getenv per backend request, which is negligible.
type staticEnvProvider struct {
	userEnv     string
	passwordEnv string
}

func newStaticEnv(userEnv, passwordEnv string) (Provider, error) {
	if userEnv == "" {
		return nil, errors.New("staticenv: user_env is required")
	}
	if passwordEnv == "" {
		return nil, errors.New("staticenv: password_env is required")
	}
	return &staticEnvProvider{userEnv: userEnv, passwordEnv: passwordEnv}, nil
}

// Credential reads the named env vars on every call. If either is
// unset or empty the caller sees the empty Credential and the
// backend's WWW-Authenticate response surfaces the failure.
func (p *staticEnvProvider) Credential(_ context.Context, _ string) (Credential, error) {
	user := os.Getenv(p.userEnv)
	password := os.Getenv(p.passwordEnv)
	if user == "" || password == "" {
		return Credential{}, nil
	}
	return Credential{Username: user, Password: password}, nil
}

// dockerConfigProvider wraps oras-go's credential store so docker-
// format config files (and credential helpers like
// docker-credential-gcr / docker-credential-ecr-login) work with no
// further ocifactory configuration.
type dockerConfigProvider struct {
	store orascreds.Store
}

func newDockerConfig(path string) (Provider, error) {
	var (
		store orascreds.Store
		err   error
	)
	if path == "" {
		store, err = orascreds.NewStoreFromDocker(orascreds.StoreOptions{})
	} else {
		store, err = orascreds.NewStore(path, orascreds.StoreOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("dockerconfig: open store: %w", err)
	}
	return &dockerConfigProvider{store: store}, nil
}

// Credential delegates to the underlying credential store and
// translates oras-go's auth.Credential into our type. Hosts with no
// matching entry yield the empty Credential, matching docker's
// behaviour and oras-go's contract.
func (p *dockerConfigProvider) Credential(ctx context.Context, host string) (Credential, error) {
	c, err := p.store.Get(ctx, host)
	if err != nil {
		return Credential{}, fmt.Errorf("dockerconfig: get %q: %w", host, err)
	}
	return Credential{
		Username:    c.Username,
		Password:    c.Password,
		AccessToken: c.AccessToken,
	}, nil
}
