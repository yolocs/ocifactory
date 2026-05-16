// Package filter is the proxy governance layer. It defines the
// [Filter] interface a proxy namespace runs against each upstream
// reference and the [Chain] composition that runs filters in order.
// In-tree filters cover the v1 needs: name [Allowlist], name
// [Denylist], and a publish-time [Delay].
//
// Filters are run as early as their inputs allow. Name-only filters
// run before any upstream call; metadata-dependent filters (e.g.
// [Delay]) return [DecisionNeedsMoreData] when they don't have what
// they need yet, and the chain caller re-runs after fetching the
// upstream metadata. See https://github.com/yolocs/ocifactory/issues/118
// for the design.
package filter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidFilter is the sentinel for filter construction /
// validation failures (invalid glob pattern, non-positive delay,
// unknown kind). Wrapped via fmt.Errorf("...: %w", ErrInvalidFilter).
var ErrInvalidFilter = errors.New("invalid filter")

// Ref identifies a package and (optionally) a specific version that
// a proxy filter is being asked to decide on. Empty fields signal
// "not known yet" — filters that depend on them should return
// [DecisionNeedsMoreData] rather than denying.
type Ref struct {
	// Package is the upstream package identifier — PyPI normalized
	// name, npm name (including any "@scope/" prefix), or Maven
	// "groupId:artifactId".
	Package string

	// Version is the upstream version string when resolved. Empty
	// when the request hasn't pinned a version yet (e.g. an index
	// fetch).
	Version string

	// UploadTime is when the version was published upstream. Zero
	// when not yet known; filters depending on it return
	// [DecisionNeedsMoreData] in that case.
	UploadTime time.Time
}

// Decision is the outcome of a single [Filter.Allow] call.
type Decision int

const (
	// DecisionAllow lets the [Ref] pass this filter. The chain
	// continues to the next filter; a chain that ends with every
	// filter returning DecisionAllow allows the request overall.
	DecisionAllow Decision = iota

	// DecisionDeny rejects the [Ref]. The chain short-circuits and
	// the caller surfaces a 404 (with a deny log + counter at the
	// caller layer).
	DecisionDeny

	// DecisionNeedsMoreData signals the filter could not evaluate
	// with the data on the [Ref] (e.g. [Delay] without an
	// UploadTime). The chain skips this filter for now and the
	// caller re-runs the chain after fetching upstream metadata.
	DecisionNeedsMoreData
)

// String makes Decision values readable in test failures and logs.
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionNeedsMoreData:
		return "needs-more-data"
	default:
		return fmt.Sprintf("decision(%d)", int(d))
	}
}

// Filter is a single check in the proxy governance chain.
//
// Implementations must be safe for concurrent use: a single Filter
// value is shared across every request that hits a namespace and is
// expected to be free of per-call mutable state.
type Filter interface {
	// Allow decides whether ref should pass this filter. Returning
	// a non-nil error is reserved for genuinely unexpected failures
	// (a Filter that wants to deny on a normal condition returns
	// DecisionDeny with a nil error); the chain treats a non-nil
	// error as fatal and propagates it.
	Allow(ctx context.Context, ref Ref) (Decision, error)
}

// Kinded is the optional extension implemented by every in-tree
// filter. It exposes the JSON discriminator value so callers
// (metrics, structured logs) can label denies by filter kind without
// reflecting on the concrete type.
type Kinded interface {
	Kind() string
}

// Chain composes filters in order. First [DecisionDeny] wins;
// [DecisionNeedsMoreData] from one filter does not short-circuit but
// is "remembered" so the chain's overall return reflects it when no
// other filter denies.
type Chain []Filter

// Allow runs the chain.
//
// The returned Filter is the deciding filter on DecisionDeny (so
// callers can label metrics / structured logs by it) and nil
// otherwise. On DecisionNeedsMoreData the returned Filter is the
// first filter that asked for more data — useful in tests but not
// load-bearing for callers, which simply re-run the chain after
// enriching the ref.
func (c Chain) Allow(ctx context.Context, ref Ref) (Decision, Filter, error) {
	var pendingFilter Filter
	for _, f := range c {
		d, err := f.Allow(ctx, ref)
		if err != nil {
			return DecisionDeny, f, err
		}
		switch d {
		case DecisionAllow:
			// keep going
		case DecisionDeny:
			return DecisionDeny, f, nil
		case DecisionNeedsMoreData:
			if pendingFilter == nil {
				pendingFilter = f
			}
		default:
			return DecisionDeny, f, fmt.Errorf("filter %T returned unknown decision %d", f, d)
		}
	}
	if pendingFilter != nil {
		return DecisionNeedsMoreData, pendingFilter, nil
	}
	return DecisionAllow, nil, nil
}

// Filters is a serialisable ordered chain. It exists as a named
// slice (not a bare []Filter) so JSON unmarshaling can dispatch each
// element to a concrete filter implementation by kind — Go's default
// encoder can't pick a concrete type for an interface field on its
// own.
type Filters []Filter

// MarshalJSON encodes fs as a JSON array. Each element delegates to
// its concrete filter's MarshalJSON, which embeds the kind
// discriminator alongside its fields.
//
// nil and empty slices both marshal as `null`/`[]` per encoding/json's
// defaults; callers that want the field omitted from the parent
// document should set the parent's JSON tag to `omitempty`.
func (fs Filters) MarshalJSON() ([]byte, error) {
	if fs == nil {
		return []byte("null"), nil
	}
	out := make([]json.RawMessage, len(fs))
	for i, f := range fs {
		if f == nil {
			return nil, fmt.Errorf("%w: nil filter at index %d", ErrInvalidFilter, i)
		}
		b, err := json.Marshal(f)
		if err != nil {
			return nil, fmt.Errorf("filters[%d]: %w", i, err)
		}
		out[i] = b
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes fs from a JSON array. Each element is
// dispatched to a concrete filter by its "kind" field; unknown kinds
// are rejected so a body persisted by a newer binary doesn't load
// silently lossy in an older one — operators get a clear upgrade
// signal instead.
func (fs *Filters) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*fs = nil
		return nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return fmt.Errorf("filters: %w", err)
	}
	out := make(Filters, 0, len(raws))
	for i, r := range raws {
		f, err := UnmarshalFilter(r)
		if err != nil {
			return fmt.Errorf("filters[%d]: %w", i, err)
		}
		out = append(out, f)
	}
	*fs = out
	return nil
}

// Validate returns nil iff every filter in fs validates. Filters
// that don't implement an unexported validate hook are treated as
// already valid (they have no construction-time arguments to check).
func (fs Filters) Validate() error {
	for i, f := range fs {
		if v, ok := f.(interface{ validate() error }); ok {
			if err := v.validate(); err != nil {
				return fmt.Errorf("filters[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// envelope is the minimal shape every persisted filter shares. The
// concrete filter's own JSON tags add the kind-specific fields.
type envelope struct {
	Kind string `json:"kind"`
}

// UnmarshalFilter decodes a single filter object. It picks the
// concrete type by the "kind" field; unknown kinds error.
func UnmarshalFilter(data []byte) (Filter, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFilter, err)
	}
	if env.Kind == "" {
		return nil, fmt.Errorf("%w: missing kind", ErrInvalidFilter)
	}
	switch env.Kind {
	case KindAllowlist:
		var a Allowlist
		if err := json.Unmarshal(data, &a); err != nil {
			return nil, fmt.Errorf("%w: allowlist: %v", ErrInvalidFilter, err)
		}
		if err := a.validate(); err != nil {
			return nil, err
		}
		return &a, nil
	case KindDenylist:
		var d Denylist
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("%w: denylist: %v", ErrInvalidFilter, err)
		}
		if err := d.validate(); err != nil {
			return nil, err
		}
		return &d, nil
	case KindDelay:
		var d Delay
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("%w: delay: %v", ErrInvalidFilter, err)
		}
		if err := d.validate(); err != nil {
			return nil, err
		}
		return &d, nil
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidFilter, env.Kind)
	}
}
