// Package version resolves boid's own version identity, per
// docs/plans/release-onboarding.md 穴2 ("バージョン identity が存在しない").
//
// There are two independent build shapes to reconcile:
//
//   - `go install github.com/novshi-tech/boid@latest` cannot pass -ldflags,
//     so the CLI has to fall back to debug.ReadBuildInfo()'s Main.Version —
//     the module version Go itself resolved at install time.
//   - the daemon/sandbox container image is built from `COPY . .` inside
//     build/container/Dockerfile, whose build context excludes .git/ (see
//     .dockerignore) — so a plain `go build` there produces a binary whose
//     build info reports "(devel)" no matter what git tag HEAD sits on.
//     The image build bakes the real version in instead, via a build-arg
//     threaded into `go build -ldflags "-X .../internal/version.buildVersion=..."`.
//
// Version() prefers the ldflags override when present and falls back to
// debug.ReadBuildInfo() otherwise, so callers never need to know which of
// the two build shapes produced the running binary.
package version

import (
	"regexp"
	"runtime/debug"
)

// buildVersion is set at image-build time via:
//
//	-ldflags "-X github.com/novshi-tech/boid/internal/version.buildVersion=vX.Y.Z"
//
// (build/container/Dockerfile's BOID_VERSION build-arg). `go install` has no
// way to set this, so it stays empty for every CLI binary installed that
// way — Version() falls back to debug.ReadBuildInfo() in that case, see its
// doc comment.
var buildVersion string

// exactReleaseTagPattern matches ONLY a bare "vMAJOR.MINOR.PATCH" string —
// no pre-release suffix, no build metadata, no pseudo-version shape. This is
// deliberately narrower than semver itself: 穴2's rule treats anything that
// isn't exactly this shape (pseudo-version, "+dirty", "(devel)", a
// git-describe-shaped string, a pre-release tag) as a local build, on the
// grounds that a wrong guess here would send `boid start` after an image
// tag/ref that doesn't exist (and "+" is not even a legal docker tag
// character) rather than falling back to a safe local-build path.
var exactReleaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// IsExactRelease reports whether v names an exact release tag under 穴2's
// rule. See exactReleaseTagPattern's doc comment for what disqualifies a
// string.
func IsExactRelease(v string) bool {
	return exactReleaseTagPattern.MatchString(v)
}

// Version returns the effective version identity of the running boid
// binary: the ldflags-injected buildVersion when the binary was built with
// it (the image build shape), otherwise whatever debug.ReadBuildInfo()
// reports as the main module's version (the `go install` shape — see the
// package doc comment). Returns "" if neither source has anything to say,
// which callers should treat the same as any other non-release value under
// IsExactRelease.
func Version() string {
	if buildVersion != "" {
		return buildVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// IsReleaseBuild reports whether the running binary's effective Version()
// names an exact release tag. This is the check callers actually want —
// composing Version() and IsExactRelease() themselves would be equivalent,
// but this spares every call site from needing to know that.
func IsReleaseBuild() bool {
	return IsExactRelease(Version())
}
