// Package filter is the proxy governance layer. It defines the
// [Filter] interface a proxy namespace runs against each upstream
// reference and the [Filters] chain that composes them.
//
// Policy in one paragraph: an [Allowlist] match returns
// [DecisionAllow]; a [Denylist] match returns [DecisionDeny]; either
// short-circuits the chain. A list that doesn't match returns
// [DecisionAbstain] and the chain moves to the next filter. A
// metadata-dependent filter like [Delay] makes the final call on
// anything no list pre-decided. Index requests ([Ref.Version] == "")
// bypass the chain entirely — filtering applies to file downloads
// only.
//
// See [docs/proxy/filter-policy.md] for the operator-facing version.
package filter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"
)

// ErrInvalidFilter is the sentinel for filter construction /
// validation failures (invalid glob pattern, non-positive delay,
// unknown kind). Wrapped via fmt.Errorf("...: %w", ErrInvalidFilter).
var ErrInvalidFilter = errors.New("invalid filter")

// Ref identifies a package and (optionally) a specific version that
// a proxy filter is being asked to decide on.
type Ref struct {
	// Package is the upstream package identifier — PyPI normalized
	// name, npm name (including any "@scope/" prefix), or Maven
	// "groupId:artifactId".
	Package string

	// Version is the upstream version string when resolved. Empty
	// when the request is an index fetch rather than a file
	// download; [Filters.Decide] bypasses the chain in that case.
	Version string

	// UploadTime is when the version was published upstream. Zero
	// when not yet known; [Delay] returns [DecisionNeedsMoreData]
	// in that case so the caller can fetch upstream metadata and
	// re-run.
	UploadTime time.Time
}

// Rule is one entry in an [Allowlist] or [Denylist]. Both fields are
// optional individually but at least one must be set — an empty rule
// matches every ref, which is almost certainly a misconfiguration.
//
// Matching rules:
//   - Package, when set, is matched against [Ref.Package] (exact or
//     [path.Match] glob).
//   - Version, when set, is matched against [Ref.Version] (same
//     semantics) — but only when [Ref.Version] is itself non-empty.
//     An empty [Ref.Version] (which only [Filters.Decide] skips, but
//     direct callers can still pass) makes any per-version rule fall
//     back to its package check alone.
type Rule struct {
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
}

// matches reports whether ref triggers this rule.
func (r Rule) matches(ref Ref) (bool, error) {
	if r.Package != "" {
		ok, err := matchPattern(r.Package, ref.Package)
		if err != nil {
			return false, fmt.Errorf("package pattern %q: %w", r.Package, err)
		}
		if !ok {
			return false, nil
		}
	}
	if r.Version != "" && ref.Version != "" {
		ok, err := matchPattern(r.Version, ref.Version)
		if err != nil {
			return false, fmt.Errorf("version pattern %q: %w", r.Version, err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (r Rule) validate() error {
	if r.Package == "" && r.Version == "" {
		return fmt.Errorf("%w: rule must populate at least one of package or version", ErrInvalidFilter)
	}
	if r.Package != "" {
		if _, err := path.Match(r.Package, ""); err != nil {
			return fmt.Errorf("%w: rule package %q: %v", ErrInvalidFilter, r.Package, err)
		}
	}
	if r.Version != "" {
		if _, err := path.Match(r.Version, ""); err != nil {
			return fmt.Errorf("%w: rule version %q: %v", ErrInvalidFilter, r.Version, err)
		}
	}
	return nil
}

// matchPattern is the shared exact-or-glob match used by [Rule].
// Exact match wins as a fast path so a literal pattern doesn't
// depend on [path.Match]'s glob interpretation.
func matchPattern(pattern, name string) (bool, error) {
	if pattern == name {
		return true, nil
	}
	return path.Match(pattern, name)
}

// Decision is the outcome of a single [Filter.Decide] call.
type Decision int

const (
	// DecisionAllow is an explicit allow. [Filters.Decide]
	// short-circuits and the request is passed through.
	DecisionAllow Decision = iota

	// DecisionDeny is an explicit deny. [Filters.Decide]
	// short-circuits and the deciding filter is returned to the
	// caller for the deny reason.
	DecisionDeny

	// DecisionAbstain means the filter has no opinion on this ref.
	// [Filters.Decide] advances to the next filter. When every
	// filter in the chain abstains the chain returns
	// [DecisionAllow].
	DecisionAbstain

	// DecisionNeedsMoreData signals the filter could not evaluate
	// with the data on the [Ref] (e.g. [Delay] without an
	// UploadTime). [Filters.Decide] returns it so the caller can
	// fetch upstream metadata and re-run.
	DecisionNeedsMoreData
)

// String makes Decision values readable in test failures and logs.
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	case DecisionAbstain:
		return "abstain"
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
	// Decide returns one of [DecisionAllow], [DecisionDeny],
	// [DecisionAbstain], or [DecisionNeedsMoreData]. A non-nil
	// error is reserved for genuinely unexpected failures; a
	// filter denying on a normal condition returns DecisionDeny
	// with a nil error.
	Decide(ctx context.Context, ref Ref) (Decision, error)
}

// Kinded is the optional extension implemented by every in-tree
// filter. It exposes the JSON discriminator value so callers
// (metrics, structured logs, deny-reason strings) can label by
// filter kind without reflecting on the concrete type.
type Kinded interface {
	Kind() string
}

// Filters is a serialisable ordered chain. The first filter to
// return [DecisionAllow], [DecisionDeny], or [DecisionNeedsMoreData]
// wins; [DecisionAbstain] advances. An empty chain or one whose
// every filter abstained returns [DecisionAllow] overall.
//
// Index requests ([Ref.Version] == "") bypass the chain — filtering
// applies to file downloads only.
type Filters []Filter

// Decide runs the chain.
//
// The returned Filter is the deciding filter for any non-Abstain
// outcome (so callers can log it / label metrics / format a deny
// reason via [Kinded.Kind]) and nil otherwise. Index requests
// (empty [Ref.Version]) return [DecisionAllow] with a nil Filter
// without invoking any filter.
func (fs Filters) Decide(ctx context.Context, ref Ref) (Decision, Filter, error) {
	if ref.Version == "" {
		return DecisionAllow, nil, nil
	}
	for _, f := range fs {
		d, err := f.Decide(ctx, ref)
		if err != nil {
			return DecisionDeny, f, err
		}
		switch d {
		case DecisionAllow, DecisionDeny, DecisionNeedsMoreData:
			return d, f, nil
		case DecisionAbstain:
			continue
		default:
			return DecisionDeny, f, fmt.Errorf("filter %T returned unknown decision %d", f, d)
		}
	}
	return DecisionAllow, nil, nil
}

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
