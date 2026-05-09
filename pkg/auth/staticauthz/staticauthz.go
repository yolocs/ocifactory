// Package staticauthz is a config-file backed implementation of
// auth.Authorizer. Operators describe which authenticated subjects
// are allowed to perform which (repo, format, op) actions in a
// YAML file mounted at a path passed via --authz-config.
//
// The implementation is intentionally narrow: subject matchers are
// equality / regex against fields of *auth.AuthContext, action
// matchers are simple globs against (repo, format, op), evaluation
// is first-match-wins, and the default decision is deny. Operators
// who need more (negative rules, priority overrides, remote
// lookups, OPA / Casbin / Rego, ...) implement auth.Authorizer
// themselves and pass it in from a custom main.
//
// File format:
//
//	default: deny             # deny | allow (default: deny)
//	rules:
//	  - subject:
//	      issuer: https://token.actions.githubusercontent.com
//	      sub_match: "^repo:my-org/build-bot:.*"
//	    allow:
//	      - { repo: "packages/*", format: python, op: write }
//	      - { repo: "packages/*", format: python, op: read  }
//	  - subject:
//	      email: build-sa@project.iam.gserviceaccount.com
//	    allow:
//	      - { repo: "*", format: "*", op: "*" }
//
// Glob semantics for repo / format / op patterns:
//   - "*"  matches any single path segment (no "/")
//   - "**" matches across path segments (including "/")
//   - exact strings match exactly
//   - empty matcher field = match anything (equivalent to "**")
//
// Subject matchers (all optional, all must match if set):
//   - issuer: exact string match against AuthContext.Issuer
//   - sub_match: regex matched against AuthContext.ID
//   - email: exact match against AuthContext.Email
//   - claims_match: map of claim name -> regex matched against
//     the claim value (must be a string in the verified Claims map)
//
// Hot-reload. An Authorizer constructed via Load remembers its
// source path. Calling Watch(ctx) starts a goroutine that
// reloads the file when it changes, so operators can edit a
// mounted ConfigMap (or push a new release of a versioned
// policy file) without restarting the server. The reload is
// atomic at the rule-set level — concurrent Authorize calls
// either see the old rules or the new rules, never a mix —
// and a malformed reload is logged and discarded so a typo
// can't take the server's policy down. See Watch for the
// kubernetes ConfigMap symlink-swap pattern.
package staticauthz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yolocs/ocifactory/pkg/auth"
	"github.com/yolocs/ocifactory/pkg/logging"
	"gopkg.in/yaml.v3"
)

// reloadDebounce is how long Watch waits after the last fsnotify
// event before re-reading the config. kubernetes ConfigMap
// updates burst many events (new timestamped dir created, symlink
// renamed, old dir removed); the debounce coalesces them into one
// reload.
const reloadDebounce = 100 * time.Millisecond

// Default determines the fall-through decision when no rule
// matches. defaultDeny is the strongly-recommended setting.
type Default string

const (
	DefaultDeny  Default = "deny"
	DefaultAllow Default = "allow"
)

// Config is the parsed YAML structure. Exported so callers
// embedding the static authorizer in their own programs can build
// it up programmatically without round-tripping through YAML.
type Config struct {
	Default Default `yaml:"default"`
	Rules   []Rule  `yaml:"rules"`
}

// Rule pairs a subject matcher with the actions that subject is
// allowed to perform. A request is allowed by a rule when the
// subject matcher matches the AuthContext AND any entry in Allow
// matches the Action.
type Rule struct {
	Subject SubjectMatcher  `yaml:"subject"`
	Allow   []ActionMatcher `yaml:"allow"`
}

// SubjectMatcher matches against an *auth.AuthContext. A field
// left empty is treated as "don't care" — only set fields
// participate in matching, and all set fields must match for the
// matcher to fire. An empty SubjectMatcher (every field zero)
// matches every authenticated subject.
type SubjectMatcher struct {
	Issuer      string            `yaml:"issuer"`
	SubMatch    string            `yaml:"sub_match"`
	Email       string            `yaml:"email"`
	ClaimsMatch map[string]string `yaml:"claims_match"`
}

// ActionMatcher matches against an auth.Action. Every field
// supports glob ("*", "**") or exact match; an empty field means
// "any". Op may also be the literal string "read", "write", or "*".
type ActionMatcher struct {
	Repo   string `yaml:"repo"`
	Format string `yaml:"format"`
	Op     string `yaml:"op"`
}

// Authorizer is the auth.Authorizer implementation. Construct via
// New (from raw Config) or Load (from a YAML file on disk). The
// compiled rule set is held in an atomic.Pointer so Watch can
// swap it out under live traffic without locking the Authorize
// fast path.
type Authorizer struct {
	rules atomic.Pointer[ruleset]

	// path is the config file path captured by Load. Empty for
	// authorizers built via New — Watch refuses to start in that
	// case because there is no source file to re-read.
	path string
}

// ruleset is the immutable compiled view of a Config. Reload
// builds a new ruleset and atomically swaps it into Authorizer.
type ruleset struct {
	def   Default
	rules []compiledRule
}

type compiledRule struct {
	subject compiledSubject
	allow   []compiledAction
}

type compiledSubject struct {
	hasIssuer   bool
	issuer      string
	hasSubMatch bool
	subMatch    *regexp.Regexp
	hasEmail    bool
	email       string
	claims      []compiledClaim
}

type compiledClaim struct {
	name string
	re   *regexp.Regexp
}

type compiledAction struct {
	repo   *globPattern
	format *globPattern
	op     *globPattern
}

// Load reads the YAML config at path, parses it, validates it,
// compiles every regex / glob, and returns an Authorizer ready
// to answer Authorize calls. The path is remembered so a
// subsequent Watch(ctx) can pick up changes.
func Load(path string) (*Authorizer, error) {
	if path == "" {
		return nil, errors.New("staticauthz: empty config path")
	}
	rs, err := loadRuleset(path)
	if err != nil {
		return nil, err
	}
	a := &Authorizer{path: path}
	a.rules.Store(rs)
	return a, nil
}

// New builds an Authorizer from an in-memory Config. Useful for
// tests and for callers building rules programmatically. An
// Authorizer built via New has no source path, so Watch returns
// an error if called on it.
func New(cfg Config) (*Authorizer, error) {
	rs, err := compileRuleset(cfg)
	if err != nil {
		return nil, err
	}
	a := &Authorizer{}
	a.rules.Store(rs)
	return a, nil
}

// Path returns the source path captured by Load. Empty for
// authorizers built via New.
func (a *Authorizer) Path() string { return a.path }

func loadRuleset(path string) (*ruleset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("staticauthz: read %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("staticauthz: parse %q: %w", path, err)
	}
	rs, err := compileRuleset(cfg)
	if err != nil {
		return nil, fmt.Errorf("staticauthz: invalid config %q: %w", path, err)
	}
	return rs, nil
}

func compileRuleset(cfg Config) (*ruleset, error) {
	def := cfg.Default
	switch def {
	case "":
		def = DefaultDeny
	case DefaultDeny, DefaultAllow:
	default:
		return nil, fmt.Errorf("default %q is not allowed (must be %q or %q)", def, DefaultDeny, DefaultAllow)
	}

	rules := make([]compiledRule, 0, len(cfg.Rules))
	for i, r := range cfg.Rules {
		cr, err := compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("rule[%d]: %w", i, err)
		}
		rules = append(rules, cr)
	}
	return &ruleset{def: def, rules: rules}, nil
}

func compileRule(r Rule) (compiledRule, error) {
	cs, err := compileSubject(r.Subject)
	if err != nil {
		return compiledRule{}, fmt.Errorf("subject: %w", err)
	}
	allow := make([]compiledAction, 0, len(r.Allow))
	for j, a := range r.Allow {
		ca, err := compileAction(a)
		if err != nil {
			return compiledRule{}, fmt.Errorf("allow[%d]: %w", j, err)
		}
		allow = append(allow, ca)
	}
	return compiledRule{subject: cs, allow: allow}, nil
}

func compileSubject(s SubjectMatcher) (compiledSubject, error) {
	out := compiledSubject{}
	if s.Issuer != "" {
		out.hasIssuer = true
		out.issuer = s.Issuer
	}
	if s.SubMatch != "" {
		re, err := regexp.Compile(s.SubMatch)
		if err != nil {
			return compiledSubject{}, fmt.Errorf("sub_match %q: %w", s.SubMatch, err)
		}
		out.hasSubMatch = true
		out.subMatch = re
	}
	if s.Email != "" {
		out.hasEmail = true
		out.email = s.Email
	}
	for name, expr := range s.ClaimsMatch {
		if name == "" {
			return compiledSubject{}, errors.New("claims_match: empty claim name")
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return compiledSubject{}, fmt.Errorf("claims_match[%q] %q: %w", name, expr, err)
		}
		out.claims = append(out.claims, compiledClaim{name: name, re: re})
	}
	return out, nil
}

func compileAction(a ActionMatcher) (compiledAction, error) {
	repo, err := compileGlob(a.Repo)
	if err != nil {
		return compiledAction{}, fmt.Errorf("repo %q: %w", a.Repo, err)
	}
	format, err := compileGlob(a.Format)
	if err != nil {
		return compiledAction{}, fmt.Errorf("format %q: %w", a.Format, err)
	}
	op, err := compileGlob(a.Op)
	if err != nil {
		return compiledAction{}, fmt.Errorf("op %q: %w", a.Op, err)
	}
	return compiledAction{repo: repo, format: format, op: op}, nil
}

// Authorize implements auth.Authorizer. It walks rules in order;
// the first rule whose subject matches and whose Allow list
// includes the action wins. If no rule matches, the configured
// default decides.
func (a *Authorizer) Authorize(_ context.Context, ac *auth.AuthContext, act auth.Action) error {
	if ac == nil {
		// Defensive — auth.Check filters this out, but a caller
		// invoking the authorizer directly shouldn't crash.
		return auth.ErrUnauthorized
	}
	rs := a.rules.Load()
	for _, r := range rs.rules {
		if !r.subject.matches(ac) {
			continue
		}
		for _, allow := range r.allow {
			if allow.matches(act) {
				return nil
			}
		}
	}
	if rs.def == DefaultAllow {
		return nil
	}
	return auth.ErrUnauthorized
}

// Watch starts a goroutine that reloads the config when its
// source file changes. Returns nil immediately on success — the
// watcher runs until ctx is canceled. Returns an error
// synchronously if the authorizer was built via New (no source
// path) or fsnotify failed to initialize.
//
// Watch is designed for kubernetes ConfigMap volume mounts. K8s
// updates a ConfigMap by writing a new timestamped directory
// alongside the existing one and atomically renaming the
// "..data" symlink — the file path operators reference
// (/etc/ocifactory/authz.yaml) is itself a symlink to
// "..data/authz.yaml", so its inode changes on every update and
// fsnotify on the file path alone misses the swap. We watch the
// containing directory, debounce the burst of events, and
// re-read the original path so the symlink chain resolves to the
// new file.
//
// Reload errors (file unreadable, YAML parse failure, regex
// invalid) are logged at WARN and discarded — the previously
// loaded rules stay in effect so a typo on disk doesn't take
// authorization down.
func (a *Authorizer) Watch(ctx context.Context) error {
	if a.path == "" {
		return errors.New("staticauthz: Watch requires Authorizer built via Load (no source path on this instance)")
	}
	abs, err := filepath.Abs(a.path)
	if err != nil {
		return fmt.Errorf("staticauthz: resolve %q: %w", a.path, err)
	}
	dir := filepath.Dir(abs)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("staticauthz: fsnotify: %w", err)
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("staticauthz: watch %q: %w", dir, err)
	}

	logger := logging.FromContext(ctx)
	go a.watchLoop(ctx, w, logger)
	return nil
}

// watchLoop is the goroutine spun up by Watch. It coalesces the
// burst of fsnotify events k8s emits during a ConfigMap rotation
// into a single reload after reloadDebounce of quiet, and exits
// when ctx is canceled.
func (a *Authorizer) watchLoop(ctx context.Context, w *fsnotify.Watcher, logger *slog.Logger) {
	defer w.Close()

	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.Events:
			if !ok {
				return
			}
			// Any event in the directory could indicate the
			// file was rewritten in place or that the k8s
			// "..data" symlink was swapped. We don't filter on
			// event Name because the symlink chain hides the
			// actual filename being touched; reloading is
			// cheap and idempotent.
			if timer == nil {
				timer = time.NewTimer(reloadDebounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(reloadDebounce)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			a.reload(ctx, logger)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			logger.WarnContext(ctx, "staticauthz: fsnotify error", "error", err, "path", a.path)
		}
	}
}

// reload re-reads the source file and atomically swaps the
// rule set on success. Failures are logged and the previous
// rule set stays in effect.
func (a *Authorizer) reload(ctx context.Context, logger *slog.Logger) {
	rs, err := loadRuleset(a.path)
	if err != nil {
		logger.WarnContext(ctx, "staticauthz: reload failed; keeping previous policy", "error", err, "path", a.path)
		return
	}
	a.rules.Store(rs)
	logger.InfoContext(ctx, "staticauthz: policy reloaded", "path", a.path, "rules", len(rs.rules))
}

func (s compiledSubject) matches(ac *auth.AuthContext) bool {
	if s.hasIssuer && s.issuer != ac.Issuer {
		return false
	}
	if s.hasSubMatch && !s.subMatch.MatchString(ac.ID) {
		return false
	}
	if s.hasEmail && s.email != ac.Email {
		return false
	}
	for _, c := range s.claims {
		raw, ok := ac.Claims[c.name]
		if !ok {
			return false
		}
		v, ok := raw.(string)
		if !ok {
			return false
		}
		if !c.re.MatchString(v) {
			return false
		}
	}
	return true
}

func (a compiledAction) matches(act auth.Action) bool {
	if !a.repo.match(act.Repo) {
		return false
	}
	if !a.format.match(act.Format) {
		return false
	}
	if !a.op.match(string(act.Op)) {
		return false
	}
	return true
}

// globPattern is a tiny "*" / "**" matcher. "*" matches any run
// of characters that does not contain "/"; "**" matches anything,
// including "/". An empty pattern matches anything (equivalent to
// "**"). Other characters match literally.
//
// A full glob library would be overkill — operators write these
// by hand and only need a handful of shapes (prefix wildcards,
// scoped namespaces). The semantics are documented in
// docs/auth.md so users know what to expect.
type globPattern struct {
	re *regexp.Regexp
}

func compileGlob(pat string) (*globPattern, error) {
	if pat == "" {
		// Match anything — the unset / "don't care" case.
		return &globPattern{re: regexp.MustCompile("^.*$")}, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				b.WriteString(".*")
				i++ // consume the second '*'
			} else {
				b.WriteString("[^/]*")
			}
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\', '?':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("compile glob: %w", err)
	}
	return &globPattern{re: re}, nil
}

func (g *globPattern) match(s string) bool {
	return g.re.MatchString(s)
}
