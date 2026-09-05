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
	// found under (an explicit v0 restriction).
	Version string
	// Dir is the absolute directory containing this Pack version's
	// integration.yaml — the bind-mount source a derived trigger uses to
	// mount the Pack read-only into a job sandbox.
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
// metadata.version agree with the directory it was installed under — a
// mismatch in either direction is a v0 startup error, never silently
// trusted.
//
// dir not existing at all is NOT an error — it returns (nil, nil), the
// same as an empty dir: the default integrations.dir
// (/opt/boid/integrations) will not exist on a deployment with no Packs
// installed yet, and that is not a misconfiguration. Every OTHER failure —
// a version directory with no integration.yaml at all, a manifest that
// fails ParseManifest, a name/version mismatch against the installation
// directory — IS a hard error naming the offending path, matching how
// config.yaml's own services: entries fail config.Load() loudly rather
// than being skipped.
//
// dir's own immediate children that are not directories (e.g. a stray
// README.md) are skipped; the expected layout is a curated directory
// containing ONLY Pack subdirectories (`/opt/boid/integrations/<pack>/
// <version>/`), not an arbitrary repo checkout root.
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
		// "boid" is reserved: boid's own internal-action signals ingest
		// under source.pack="boid" (signal_ingest_bridge.go's
		// InternalSignalPack, internal/orchestrator), so no REAL installed
		// Pack may ever claim that name — a collision would let an
		// external Pack's rows masquerade as (or shadow) internal signals
		// under the same signals table PRIMARY KEY.
		if packName == "boid" {
			return nil, fmt.Errorf("integrationpack: %s: pack name %q is reserved for boid's own internal-signal source and cannot be used by an installed Pack", filepath.Join(dir, packName), packName)
		}
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
