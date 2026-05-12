// Package version exposes the binary's build identity. Release
// pipelines override Version and Commit at link time via
// -ldflags="-X github.com/yolocs/ocifactory/internal/version.Version=...";
// any field left empty by the linker falls back to abcxyz/pkg/buildinfo,
// which reads runtime/debug.ReadBuildInfo (vcs.revision and the
// module version) so dev builds still surface their commit instead of
// a "dev" placeholder.
package version

import (
	"runtime"

	"github.com/abcxyz/pkg/buildinfo"
)

// Name is the binary name. Constant — release pipelines override
// Version/Commit but never rename the binary.
const Name = "ocifactory"

// Version, Commit, and OSArch are package-level string variables with
// no initializer so the linker's -X flag can set them at
// release-build time. Go's -X only overrides strings initialized to a
// constant or left blank — `var Version = buildinfo.Version()` would
// silently prevent the override, which is why the buildinfo defaults
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
// runtime-derived defaults (buildinfo / GOOS+GOARCH) and composes the
// HumanVersion line. Exposed as a non-exported helper so tests can
// exercise the LDFLAGS-override path (which is otherwise unreachable
// from a unit test) by passing non-empty inputs.
func resolve(version, commit, osArch string) (v, c, oa, human string) {
	v, c, oa = version, commit, osArch
	if v == "" {
		v = buildinfo.Version()
	}
	if c == "" {
		c = buildinfo.Commit()
	}
	if oa == "" {
		oa = runtime.GOOS + "/" + runtime.GOARCH
	}
	human = Name + " " + v + " (commit " + c + ", " + oa + ")"
	return v, c, oa, human
}
