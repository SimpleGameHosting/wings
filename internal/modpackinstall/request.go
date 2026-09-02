// Package modpackinstall holds the pure file-and-network semantics of the
// native modpack/version installer: request validation, clean profiles,
// hardened download, staged extraction and settle, and finalize rules.
// It deliberately never imports package server so it unit-tests in isolation.
package modpackinstall

import (
	"net/url"

	"emperror.dev/errors"
	"github.com/google/uuid"
)

type Kind string

const (
	KindModpack Kind = "modpack"
	KindVersion Kind = "version"
)

type VersionType string

const (
	VersionVanilla  VersionType = "vanilla"
	VersionSnapshot VersionType = "snapshot"
	VersionNeoForge VersionType = "neoforge"
	VersionPaper    VersionType = "paper"
	VersionPurpur   VersionType = "purpur"
	VersionSponge   VersionType = "sponge"
	VersionVelocity VersionType = "velocity"
	VersionForge    VersionType = "forge"
	VersionFabric   VersionType = "fabric"
)

var knownVersionTypes = map[VersionType]struct{}{
	VersionVanilla: {}, VersionSnapshot: {}, VersionNeoForge: {}, VersionPaper: {},
	VersionPurpur: {}, VersionSponge: {}, VersionVelocity: {}, VersionForge: {},
	VersionFabric: {},
}

const (
	// TempArchiveName is the fixed on-disk name of the downloading archive;
	// both clean profiles always remove it so crashes cannot strand bytes.
	TempArchiveName = ".sgh-install.tmp"

	// StagingDirName is where archives extract before settling to the root.
	StagingDirName = ".sgh-install-work"

	// FormatJar marks a raw jar artifact that must not be extracted.
	FormatJar = "jar"
)

// Request is the wire payload of POST /api/servers/:server/modpack-install.
type Request struct {
	InstallID     string      `json:"install_id"`
	Kind          Kind        `json:"kind"`
	DownloadURL   string      `json:"download_url"`
	ArchiveFormat string      `json:"archive_format"`
	VersionType   VersionType `json:"version_type"`
	ModpackID     string      `json:"modpack_id"`
	VersionID     string      `json:"version_id"`
}

// Validate rejects anything the pipeline is not written to handle. Unknown
// values are refused outright; there are deliberately no default behaviors.
func (r *Request) Validate() error {
	if _, err := uuid.Parse(r.InstallID); err != nil {
		return errors.New("modpackinstall: install_id must be a UUID")
	}

	if r.Kind != KindModpack && r.Kind != KindVersion {
		return errors.Errorf("modpackinstall: unknown kind %q", r.Kind)
	}

	u, err := url.Parse(r.DownloadURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("modpackinstall: download_url must be an absolute http(s) URL")
	}

	if r.ArchiveFormat != "" && r.ArchiveFormat != FormatJar {
		return errors.Errorf("modpackinstall: archive_format may only be omitted or %q; archives are sniffed", FormatJar)
	}

	// A modpack is always a packaged tree, never a single runtime jar, so
	// the jar format is refused here rather than quietly ignored...
	if r.Kind == KindModpack && r.ArchiveFormat == FormatJar {
		return errors.Errorf("modpackinstall: archive_format %q is only valid for kind=%q", FormatJar, KindVersion)
	}

	if r.Kind == KindVersion {
		if _, ok := knownVersionTypes[r.VersionType]; !ok {
			return errors.Errorf("modpackinstall: unknown version_type %q", r.VersionType)
		}
	} else if r.VersionType != "" {
		return errors.New("modpackinstall: version_type is only valid for kind=version")
	}

	return nil
}
