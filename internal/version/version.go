// Package version exposes the binary's build identity. Release
// pipelines override Version and Commit at link time via
// -ldflags="-X github.com/yolocs/ocifactory/internal/version.Version=...";
// any field left empty by the linker falls back to
// runtime/debug.ReadBuildInfo (vcs.revision and the module version)
// so dev builds still surface their commit instead of a "dev"
// placeholder.
package version

import (
	"runtime"
	"runtime/debug"
)

// Name is the binary name. Constant — release pipelines override
// Version/Commit but never rename the binary.
const Name = "ocifactory"

// Version, Commit, and OSArch are package-level string variables with
// no initializer so the linker's -X flag can set them at
// release-build time. Go's -X only overrides strings initialized to a
// constant or left blank — `var Version = readModuleVersion()` would
// silently prevent the override, which is why the runtime defaults
// are applied in init() rather than inline.
var (
	Version string
	Commit  string
	OSArch  string
)

// HumanVersion is the single-line build identity shown by --version
// and surfaced in /readyz. Composed in init() so it picks up LDFLAGS
// overrides on Version/Commit before formatting.
var HumanVersion string

func init() {
	Version, Commit, OSArch, HumanVersion = resolve(Version, Commit, OSArch)
}

// resolve fills in any empty build-identity field from
// runtime-derived defaults and composes the HumanVersion line.
// Exposed as a non-exported helper so tests can exercise the
// LDFLAGS-override path (which is otherwise unreachable from a unit
// test) by passing non-empty inputs.
func resolve(version, commit, osArch string) (v, c, oa, human string) {
	v, c, oa = version, commit, osArch
	if v == "" {
		v = readModuleVersion()
	}
	if c == "" {
		c = readVCSRevision()
	}
	if oa == "" {
		oa = runtime.GOOS + "/" + runtime.GOARCH
	}
	human = Name + " " + v + " (commit " + c + ", " + oa + ")"
	return v, c, oa, human
}

// readModuleVersion returns the module version embedded by the
// compiler (e.g. "v1.2.3" for `go install`-ed binaries, "(devel)" for
// `go build` from a working tree). Falls back to "source" when no
// build info is present, matching the abcxyz/pkg/buildinfo behaviour
// the original design referenced.
func readModuleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" {
			return v
		}
	}
	return "source"
}

// readVCSRevision returns the git SHA the compiler stamped via
// vcs.revision (enabled by default since Go 1.18 / -buildvcs=true).
// "HEAD" when unavailable — e.g. binaries built outside a checkout.
func readVCSRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "HEAD"
}
