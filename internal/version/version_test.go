package version

import "testing"

// TestIsExactRelease pins docs/plans/release-onboarding.md 穴2's判定ルール:
// "exact release tag 以外 (pseudo-version / +dirty サフィックス / (devel))
// はすべてローカルビルド経路として広く扱う。exact release tag だけをリリース
// 版とみなす。" — the rule is deliberately broad/conservative: anything that
// is not a bare "vMAJOR.MINOR.PATCH" string is treated as a local build,
// never guessed at.
func TestIsExactRelease(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"exact tag", "v0.0.13", true},
		{"exact tag, single digit", "v1.2.3", true},
		{"exact tag, multi-digit components", "v10.20.300", true},
		{"empty string", "", false},
		{"devel marker", "(devel)", false},
		{"pseudo-version, no prior tag", "v0.0.0-20260801120000-abcdef123456", false},
		{"pseudo-version, after a tag", "v0.0.13-0.20260801120000-abcdef123456", false},
		{"dirty suffix on exact tag", "v0.0.13+dirty", false},
		{"dirty suffix on pseudo-version", "v0.0.13-0.20260801120000-abcdef123456+dirty", false},
		{"git-describe-shaped string", "v0.0.13-338-g38b6162", false},
		{"prerelease suffix", "v0.0.13-rc1", false},
		{"missing v prefix", "0.0.13", false},
		{"build metadata only", "v0.0.13+meta", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExactRelease(tc.in); got != tc.want {
				t.Errorf("IsExactRelease(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestVersion_PrefersBuildVersionOverride pins the image-side half of 穴2:
// the ldflags -X override (baked into the image via
// build/container/Dockerfile's BOID_VERSION build-arg) must win over
// whatever debug.ReadBuildInfo() would otherwise report, since a `go build`
// inside the image builder stage stamps VCS info that is not the release
// identity we want image binaries to report (see 穴2's "image 側" note:
// .dockerignore excludes .git/ and the Dockerfile's build has no ldflags
// today, so the image binary is `(devel)`-shaped without this override).
func TestVersion_PrefersBuildVersionOverride(t *testing.T) {
	orig := buildVersion
	defer func() { buildVersion = orig }()

	buildVersion = "v0.0.13"
	if got := Version(); got != "v0.0.13" {
		t.Errorf("Version() = %q, want %q", got, "v0.0.13")
	}
}

// TestVersion_FallsBackToBuildInfoWhenNoOverride pins the go-install/CLI
// half of 穴2: with no ldflags override, Version() must not panic and must
// return a string (whatever debug.ReadBuildInfo().Main.Version reports in
// this test binary — content is environment-dependent, so only the
// no-override code path is exercised here, not a specific value).
func TestVersion_FallsBackToBuildInfoWhenNoOverride(t *testing.T) {
	orig := buildVersion
	defer func() { buildVersion = orig }()

	buildVersion = ""
	_ = Version()
}

// TestIsReleaseBuild pins that IsReleaseBuild is exactly IsExactRelease
// applied to the effective Version(), so callers don't have to remember to
// compose the two themselves.
func TestIsReleaseBuild(t *testing.T) {
	orig := buildVersion
	defer func() { buildVersion = orig }()

	buildVersion = "v0.0.13"
	if !IsReleaseBuild() {
		t.Errorf("IsReleaseBuild() = false, want true for exact tag %q", buildVersion)
	}

	buildVersion = "v0.0.13+dirty"
	if IsReleaseBuild() {
		t.Errorf("IsReleaseBuild() = true, want false for dirty build %q", buildVersion)
	}
}
