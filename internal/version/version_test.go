package version

import (
	"strings"
	"testing"
)

func TestBuildIdentity_NonEmpty(t *testing.T) {
	t.Parallel()

	// Default values come from runtime/debug.ReadBuildInfo when no
	// LDFLAGS overrides are present. Even un-flagged builds should
	// produce non-empty Version/Commit/OSArch so /readyz and
	// --version are never bare placeholders.
	tests := []struct {
		name string
		got  string
	}{
		{name: "Name", got: Name},
		{name: "Version", got: Version},
		{name: "Commit", got: Commit},
		{name: "OSArch", got: OSArch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got == "" {
				t.Errorf("%s = %q, want non-empty", tc.name, tc.got)
			}
		})
	}
}

func TestHumanVersion_Composition(t *testing.T) {
	t.Parallel()

	// HumanVersion is what `ocifactory --version` and /readyz surface.
	// It must include every piece operators rely on to triage rolling
	// deploys: program name, semver, commit, and OS/arch.
	tests := []struct {
		name      string
		substring string
	}{
		{name: "includes program name", substring: Name},
		{name: "includes version", substring: Version},
		{name: "includes commit", substring: Commit},
		{name: "includes os/arch", substring: OSArch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(HumanVersion, tc.substring) {
				t.Errorf("HumanVersion = %q, want substring %q", HumanVersion, tc.substring)
			}
		})
	}
}

func TestName_IsStable(t *testing.T) {
	t.Parallel()
	// Name is the constant binary name; release pipelines rename
	// neither the binary nor this constant. Pinning it stops a future
	// rename from quietly skewing the --version output.
	if Name != "ocifactory" {
		t.Errorf("Name = %q, want %q", Name, "ocifactory")
	}
}

func TestResolve_RespectsLDFLAGSOverrides(t *testing.T) {
	t.Parallel()

	// resolve() is the seam between -X-injected values and runtime
	// defaults. At link time, LDFLAGS land on Version/Commit before
	// init() runs; init() then calls resolve() with those values. The
	// table below simulates that contract: non-empty inputs must pass
	// through untouched (release builds), empty inputs must be filled
	// from the runtime defaults (local builds).
	tests := []struct {
		name              string
		inVersion         string
		inCommit          string
		inOSArch          string
		wantVersion       string
		wantCommit        string
		wantOSArch        string
		wantHumanContains []string
	}{
		{
			name:              "all overridden (release build)",
			inVersion:         "v1.2.3",
			inCommit:          "deadbeef",
			inOSArch:          "linux/arm64",
			wantVersion:       "v1.2.3",
			wantCommit:        "deadbeef",
			wantOSArch:        "linux/arm64",
			wantHumanContains: []string{"ocifactory", "v1.2.3", "deadbeef", "linux/arm64"},
		},
		{
			name:              "only version overridden",
			inVersion:         "v9.9.9",
			wantVersion:       "v9.9.9",
			wantHumanContains: []string{"ocifactory", "v9.9.9"},
		},
		{
			name:              "nothing overridden (dev build)",
			wantHumanContains: []string{"ocifactory"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotV, gotC, gotOA, gotHuman := resolve(tc.inVersion, tc.inCommit, tc.inOSArch)

			if tc.wantVersion != "" && gotV != tc.wantVersion {
				t.Errorf("Version = %q, want %q", gotV, tc.wantVersion)
			}
			if tc.wantCommit != "" && gotC != tc.wantCommit {
				t.Errorf("Commit = %q, want %q", gotC, tc.wantCommit)
			}
			if tc.wantOSArch != "" && gotOA != tc.wantOSArch {
				t.Errorf("OSArch = %q, want %q", gotOA, tc.wantOSArch)
			}
			if gotV == "" || gotC == "" || gotOA == "" {
				t.Errorf("resolve returned empty field: Version=%q Commit=%q OSArch=%q", gotV, gotC, gotOA)
			}
			for _, sub := range tc.wantHumanContains {
				if !strings.Contains(gotHuman, sub) {
					t.Errorf("HumanVersion = %q, want substring %q", gotHuman, sub)
				}
			}
		})
	}
}
