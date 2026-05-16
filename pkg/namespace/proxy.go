package namespace

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/yolocs/ocifactory/pkg/proxy/filter"
)

// ModeHosted is the default namespace mode. A hosted namespace stores
// artifacts uploaded by clients and serves them back; it never reaches
// out to an upstream registry. An empty [Spec.Mode] resolves to
// [ModeHosted] so namespaces written before this field was introduced
// continue to load unchanged.
const ModeHosted = "hosted"

// ModeProxy is the pull-through proxy namespace mode. A proxy namespace
// fetches artifacts on demand from [Proxy.Upstream] and caches them in
// the same OCI-backed shape as a hosted namespace.
const ModeProxy = "proxy"

// ErrInvalidProxy is the sentinel for [Proxy.Validate] failures. The
// admin handler maps it to HTTP 400.
var ErrInvalidProxy = errors.New("invalid proxy")

// Proxy is the pull-through proxy block of a [Spec]. It is only
// honored when [Spec.Mode] is [ModeProxy]; presence on a hosted
// namespace is a validation error.
type Proxy struct {
	// Upstream is the canonical URL of the upstream registry this
	// namespace mirrors (e.g. "https://pypi.org",
	// "https://registry.npmjs.org", "https://repo1.maven.org/maven2").
	// Required when [Spec.Mode] is [ModeProxy]; the value must parse
	// as an absolute URL.
	Upstream string `json:"upstream,omitempty"`

	// Filters is the ordered filter chain applied before any upstream
	// call. First deny wins; [filter.DecisionNeedsMoreData] from a
	// metadata-dependent filter is re-run after upstream metadata
	// fetch.
	Filters filter.Filters `json:"filters,omitempty"`
}

// Filter is one element of the proxy filter chain. Concrete shapes
// live in [pkg/proxy/filter]; the type alias keeps the
// namespace-package call sites readable.
type Filter = filter.Filter

// Validate returns nil iff p is consistent with mode: when mode is
// [ModeProxy], Upstream must be a parseable absolute URL; when mode is
// hosted (empty or [ModeHosted]), no proxy fields may be populated.
// Errors wrap [ErrInvalidProxy].
func (p *Proxy) Validate(mode string) error {
	if p == nil {
		p = &Proxy{}
	}
	hosted := mode == "" || mode == ModeHosted
	if hosted {
		if p.Upstream != "" || len(p.Filters) > 0 {
			return fmt.Errorf("%w: proxy block must be empty on mode %q", ErrInvalidProxy, ModeHosted)
		}
		return nil
	}
	if mode != ModeProxy {
		return fmt.Errorf("%w: unknown mode %q (want %q or %q)", ErrInvalidProxy, mode, ModeHosted, ModeProxy)
	}
	if p.Upstream == "" {
		return fmt.Errorf("%w: upstream is required on mode %q", ErrInvalidProxy, ModeProxy)
	}
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return fmt.Errorf("%w: upstream %q: %v", ErrInvalidProxy, p.Upstream, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%w: upstream %q must be an absolute URL with a host", ErrInvalidProxy, p.Upstream)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: upstream %q scheme must be http or https", ErrInvalidProxy, p.Upstream)
	}
	if err := p.Filters.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProxy, err)
	}
	return nil
}
