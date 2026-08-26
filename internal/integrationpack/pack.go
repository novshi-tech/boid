package integrationpack

import (
	"fmt"
	"os"
	"path/filepath"
)

// Pack is one loaded, validated <dir>/<pack>/<version>/integration.yaml —
// LoadPacks' unit of enumeration.
type Pack struct {
	// Name is the manifest's own metadata.name — LoadPacks has already
	// checked this equals the <pack> directory segment it was found under.
	Name string
	// Version is the manifest's own metadata.version — LoadPacks has
	// already checked this equals the <version> directory segment it was
	// found under (docs/plans/signal-ingest-detailed-design.md §6.2's
	// explicit v0 restriction).
	Version string
	// Dir is the absolute directory containing this Pack version's
	// integration.yaml — the bind-mount source PR-5's derived trigger uses
	// (docs/plans/signal-ingest-detailed-design.md §7.1 "mount 位置": Pack
	// 一式を /run/boid/integrations/<pack> へ read-only bind).
	Dir string
	// Manifest is this Pack's fully parsed, structurally-validated
	// integration.yaml content.
	Manifest Manifest
}

// ServiceProfile finds the named service profile within p.Manifest, or
// (nil, false) if p declares no profile with that name — the lookup
// DesugarService (resolve.go) uses to resolve a uses: reference's profile
// half.
func (p *Pack) ServiceProfile(name string) (*ServiceProfile, bool) {
	for i := range p.Manifest.ServiceProfiles {
		if p.Manifest.ServiceProfiles[i].Name == name {
			return &p.Manifest.ServiceProfiles[i], true
		}
	}
	return nil, false
}

// LoadPacks enumerates <dir>/<pack-name>/<version>/integration.yaml,
// strict-decoding and validating each manifest found (ParseManifest), and
// additionally checking that the manifest's own metadata.name/
// metadata.version agree with the directory it was installed under
// (docs/plans/signal-ingest-detailed-design.md §6.2: "<ver> ディレクトリ名
// が manifest の version と一致しない Pack は v0 では起動エラー" — the same
// "don't silently trust whichever is right" posture extends to name).
//
// dir not existing at all is NOT an error — it returns (nil, nil), the
// same as an empty dir: the default integrations.dir
// (/opt/boid/integrations) will not exist on a deployment with no Packs
// installed yet, and that is not a misconfiguration (docs/plans/
// signal-ingest-detailed-design.md §6.1). Every OTHER failure — a version
// directory with no integration.yaml at all, a manifest that fails
// ParseManifest, a name/version mismatch against the installation
// directory — IS a hard error naming the offending path: PR-4's "検証失敗
// は起動エラー" posture (§6.2 item 3), matching how config.yaml's own
// services: entries fail config.Load() loudly rather than being skipped.
//
// dir's own immediate children that are not directories (e.g. a stray
// README.md) are skipped; the expected layout is a curated directory
// containing ONLY Pack subdirectories (docs/plans/signal-driven-review.md
// §6.4's `/opt/boid/integrations/<pack>/<version>/` tree), not an arbitrary
// repo checkout root.
func LoadPacks(dir string) ([]*Pack, error) {
	packEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("integrationpack: read %s: %w", dir, err)
	}

	var packs []*Pack
	for _, pe := range packEntries {
		if !pe.IsDir() {
			continue
		}
		packName := pe.Name()
		packDir := filepath.Join(dir, packName)
		verEntries, err := os.ReadDir(packDir)
		if err != nil {
			return nil, fmt.Errorf("integrationpack: read %s: %w", packDir, err)
		}
		for _, ve := range verEntries {
			if !ve.IsDir() {
				continue
			}
			version := ve.Name()
			verDir := filepath.Join(packDir, version)
			manifestPath := filepath.Join(verDir, "integration.yaml")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, fmt.Errorf("integrationpack: %s: no integration.yaml found (every <pack>/<version> directory under integrations.dir must contain one)", verDir)
				}
				return nil, fmt.Errorf("integrationpack: read %s: %w", manifestPath, err)
			}
			m, err := ParseManifest(data)
			if err != nil {
				return nil, fmt.Errorf("integrationpack: %s: %w", manifestPath, err)
			}
			if m.Metadata.Name != packName {
				return nil, fmt.Errorf("integrationpack: %s: metadata.name %q does not match its installation directory %q", manifestPath, m.Metadata.Name, packName)
			}
			if m.Metadata.Version != version {
				return nil, fmt.Errorf("integrationpack: %s: metadata.version %q does not match its installation directory %q", manifestPath, m.Metadata.Version, version)
			}
			packs = append(packs, &Pack{
				Name:     m.Metadata.Name,
				Version:  m.Metadata.Version,
				Dir:      verDir,
				Manifest: *m,
			})
		}
	}
	return packs, nil
}
