package backend

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gcpadcDefaultScope is the OAuth2 scope ADC requests when the
// operator hasn't set OCIFACTORY_BACKEND_AUTH_GCPADC_SCOPES.
// cloud-platform is the universally accepted answer for any GAR /
// GCR call ocifactory makes; operators wanting a narrower scope (a
// read-only deployment, etc.) override the env var.
const gcpadcDefaultScope = "https://www.googleapis.com/auth/cloud-platform"

// gcpadcProvider mints Google access tokens via Application Default
// Credentials. ADC works on Cloud Run / GCE / GKE via the metadata
// server, locally via `gcloud auth application-default login`, and
// via Workload Identity Federation when the right environment is
// set — all transparently.
//
// host is ignored: ADC issues bearer tokens for any GCP service the
// credential has access to, regardless of registry host.
type gcpadcProvider struct {
	tokenSource oauth2.TokenSource
}

type gcpadcOptions struct {
	Scopes []string

	// tokenSource lets tests substitute a fixed source so they
	// don't reach for real Google metadata. Unexported on purpose:
	// production callers go through gcpadcOptions{Scopes: ...}.
	tokenSource oauth2.TokenSource
}

func newGCPADC(opts gcpadcOptions) (Provider, error) {
	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = []string{gcpadcDefaultScope}
	}

	ts := opts.tokenSource
	if ts == nil {
		creds, err := google.FindDefaultCredentials(context.Background(), scopes...)
		if err != nil {
			return nil, fmt.Errorf("gcpadc: find default credentials: %w", err)
		}
		ts = creds.TokenSource
	}
	return &gcpadcProvider{tokenSource: ts}, nil
}

// Credential mints a fresh access token via the configured
// TokenSource. oauth2.TokenSource handles refresh internally so
// long-lived ocifactory processes don't need their own cache.
func (p *gcpadcProvider) Credential(_ context.Context, _ string) (Credential, error) {
	tok, err := p.tokenSource.Token()
	if err != nil {
		return Credential{}, fmt.Errorf("gcpadc: get token: %w", err)
	}
	return Credential{AccessToken: tok.AccessToken}, nil
}
