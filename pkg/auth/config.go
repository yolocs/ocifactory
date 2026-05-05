package auth

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// FileSchema is the on-disk YAML shape of the auth-config file
// loaded via --auth-config. It enumerates one or more authenticators
// in the order the chain should try them.
//
// Example:
//
//	authenticators:
//	  - kind: oidc
//	    issuer: https://accounts.google.com
//	    audience: https://ocifactory.your-domain
//	  - kind: oidc
//	    issuer: https://token.actions.githubusercontent.com
//	    audience: https://ocifactory.your-domain
type FileSchema struct {
	Authenticators []AuthenticatorSpec `yaml:"authenticators"`
}

// AuthenticatorSpec is one entry in the auth-config file. Kind
// selects which factory handles the entry; the rest of the YAML
// node is captured raw so each factory can decode its own shape
// without this package having to know about it.
//
// This makes the schema extensible: an out-of-tree authenticator
// (a static-password kind, GitHub PAT validator, mTLS, ...) only
// needs to RegisterKind a factory in init() and operators add
// `kind: <name>` entries to their YAML.
type AuthenticatorSpec struct {
	Kind string

	// node holds the original YAML mapping for Decode. Not
	// exported because the consumer interface is Decode(v).
	node yaml.Node
}

// UnmarshalYAML pulls Kind out of the mapping (so the registry
// lookup in Build doesn't need to re-decode) and stores the whole
// node so factories can read their own fields via Decode.
func (s *AuthenticatorSpec) UnmarshalYAML(value *yaml.Node) error {
	s.node = *value
	var k struct {
		Kind string `yaml:"kind"`
	}
	if err := value.Decode(&k); err != nil {
		return err
	}
	s.Kind = k.Kind
	return nil
}

// Decode unmarshals the spec into v. Factories call this with
// their own struct type to read implementation-specific fields.
func (s *AuthenticatorSpec) Decode(v any) error {
	return s.node.Decode(v)
}

// Factory builds one Authenticator from its YAML spec. Implementers
// decode the spec into their own struct via spec.Decode(&yourStruct)
// and return a ready-to-use Authenticator.
type Factory func(spec AuthenticatorSpec) (Authenticator, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// RegisterKind associates a factory with a kind name. Call this
// from init() in the package that implements the kind. Re-
// registering an existing kind panics — duplicates indicate a
// configuration bug, not a runtime condition.
//
// Built-in kinds (today: "oidc") register themselves; out-of-tree
// kinds plug in by being imported for side effects:
//
//	import _ "example.com/ocifactory-staticauth"
func RegisterKind(name string, f Factory) {
	if name == "" {
		panic("auth: RegisterKind name is empty")
	}
	if f == nil {
		panic("auth: RegisterKind factory is nil")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; ok {
		panic(fmt.Sprintf("auth: RegisterKind: %q already registered", name))
	}
	registry[name] = f
}

// RegisteredKinds returns the names of all registered kinds in
// sorted order. Useful for log lines and error messages so an
// operator with a typo sees what's actually available.
func RegisteredKinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LoadConfigFile reads a YAML auth config and returns its parsed
// shape. It does not instantiate authenticators — that's Build.
func LoadConfigFile(path string) (*FileSchema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read %q: %w", path, err)
	}
	var s FileSchema
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("auth: parse %q: %w", path, err)
	}
	if len(s.Authenticators) == 0 {
		return nil, fmt.Errorf("auth: %q: no authenticators configured", path)
	}
	for i, a := range s.Authenticators {
		if a.Kind == "" {
			return nil, fmt.Errorf("auth: authenticators[%d]: kind is required", i)
		}
	}
	return &s, nil
}

// Build instantiates one Authenticator per spec via the registered
// factories and returns them in declaration order. Callers compose
// them into a Chain (or use them individually).
func Build(cfg *FileSchema) ([]Authenticator, error) {
	out := make([]Authenticator, 0, len(cfg.Authenticators))
	for i, spec := range cfg.Authenticators {
		registryMu.RLock()
		f, ok := registry[spec.Kind]
		registryMu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("auth: authenticators[%d]: unknown kind %q (registered: %v)",
				i, spec.Kind, RegisteredKinds())
		}
		a, err := f(spec)
		if err != nil {
			return nil, fmt.Errorf("auth: authenticators[%d] (%s): %w", i, spec.Kind, err)
		}
		out = append(out, a)
	}
	return out, nil
}
