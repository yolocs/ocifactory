package auth

import (
	"fmt"
	"os"

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
//	  - kind: basictoken
//	    file: /etc/ocifactory/tokens.yaml
type FileSchema struct {
	Authenticators []AuthenticatorSpec `yaml:"authenticators"`
}

// AuthenticatorSpec is one entry in the auth-config file.
// Different kinds use different fields; unrecognised fields are
// ignored by yaml.v3 by default — we surface unknown kinds as a
// hard error so typos can't silently drop an authenticator.
type AuthenticatorSpec struct {
	// Kind selects which implementation to instantiate. One of
	// "oidc" or "basictoken".
	Kind string `yaml:"kind"`

	// OIDC fields (kind == "oidc").
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`

	// Basictoken fields (kind == "basictoken").
	File string `yaml:"file"`
}

// LoadConfigFile reads a YAML auth config and returns its parsed
// shape. It does not instantiate authenticators — the serve
// command does that so it can apply per-process options
// (http.Client, etc.) without this package needing to know about
// them.
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
		switch a.Kind {
		case "oidc":
			if a.Issuer == "" {
				return nil, fmt.Errorf("auth: authenticators[%d]: oidc requires issuer", i)
			}
			if a.Audience == "" {
				return nil, fmt.Errorf("auth: authenticators[%d]: oidc requires audience", i)
			}
		case "basictoken":
			if a.File == "" {
				return nil, fmt.Errorf("auth: authenticators[%d]: basictoken requires file", i)
			}
		case "":
			return nil, fmt.Errorf("auth: authenticators[%d]: kind is required", i)
		default:
			return nil, fmt.Errorf("auth: authenticators[%d]: unknown kind %q", i, a.Kind)
		}
	}
	return &s, nil
}
