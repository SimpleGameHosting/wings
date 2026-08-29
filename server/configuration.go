package server

import (
	"maps"
	"slices"
	"sync"

	"github.com/pterodactyl/wings/environment"
)

type EggConfiguration struct {
	// The internal UUID of the Egg on the Panel.
	ID string `json:"id"`

	// Maintains a list of files that are blacklisted for opening/editing/downloading
	// or basically any type of access on the server by any user. This is NOT the same
	// as a per-user denylist, this is defined at the Egg level.
	FileDenylist []string `json:"file_denylist"`
}

type ConfigurationMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ConfigurationData holds the server settings the Panel syncs to Wings. It is a
// plain value with no lock of its own, so it can be decoded, copied, and swapped
// into place wholesale; Configuration wraps it with the lock guarding the live copy.
type ConfigurationData struct {
	// The unique identifier for the server that should be used when referencing
	// it against the Panel API (and internally). This will be used when naming
	// docker containers as well as in log output.
	Uuid string `json:"uuid"`

	Meta ConfigurationMeta `json:"meta"`

	// Whether or not the server is in a suspended state. Suspended servers cannot
	// be started or modified except in certain scenarios by an admin user.
	Suspended bool `json:"suspended"`

	// The command that should be used when booting up the server instance.
	Invocation string `json:"invocation"`

	// By default this is false, however if selected within the Panel while installing or re-installing a
	// server, specific installation scripts will be skipped for the server process.
	SkipEggScripts bool `json:"skip_egg_scripts"`

	// An array of environment variables that should be passed along to the running
	// server process.
	EnvVars environment.Variables `json:"environment"`

	// Labels is a map of container labels that should be applied to the running server process.
	Labels map[string]string `json:"labels"`

	Allocations           environment.Allocations `json:"allocations"`
	Build                 environment.Limits      `json:"build"`
	CrashDetectionEnabled bool                    `json:"crash_detection_enabled"`
	Mounts                []Mount                 `json:"mounts"`
	Egg                   EggConfiguration        `json:"egg,omitempty"`

	Container struct {
		// Defines the Docker image that will be used for this server
		Image string `json:"image,omitempty"`
	} `json:"container,omitempty"`
}

// Configuration is the live, lock-guarded configuration of a server. The settings
// live in the embedded ConfigurationData so that SyncWithConfiguration can replace
// them without ever copying the mutex.
type Configuration struct {
	mu sync.RWMutex

	ConfigurationData
}

// Config returns an isolated snapshot of the server settings. Mutating the
// returned value never changes the live server configuration.
func (s *Server) Config() *ConfigurationData {
	snapshot := s.configurationSnapshot()

	return &snapshot
}

// DiskSpace returns the amount of disk space available to a server in bytes.
func (s *Server) DiskSpace() int64 {
	s.cfg.mu.RLock()
	defer s.cfg.mu.RUnlock()
	return s.cfg.Build.DiskSpace * 1024.0 * 1024.0
}

func (s *Server) MemoryLimit() int64 {
	s.cfg.mu.RLock()
	defer s.cfg.mu.RUnlock()
	return s.cfg.Build.MemoryLimit
}

// configurationSnapshot returns a copy of the server settings taken under the
// read lock, for callers that serialize the whole configuration rather than
// read a single field through Config().
func (s *Server) configurationSnapshot() ConfigurationData {
	s.cfg.mu.RLock()
	defer s.cfg.mu.RUnlock()

	snapshot := s.cfg.ConfigurationData
	snapshot.EnvVars = maps.Clone(snapshot.EnvVars)
	snapshot.Labels = maps.Clone(snapshot.Labels)
	snapshot.Mounts = slices.Clone(snapshot.Mounts)
	snapshot.Egg.FileDenylist = slices.Clone(snapshot.Egg.FileDenylist)
	if s.cfg.Allocations.Mappings != nil {
		snapshot.Allocations.Mappings = make(map[string][]int, len(s.cfg.Allocations.Mappings))
		for address, ports := range s.cfg.Allocations.Mappings {
			snapshot.Allocations.Mappings[address] = slices.Clone(ports)
		}
	}

	return snapshot
}

// SetSuspended atomically changes the server's local suspension state.
func (s *Server) SetSuspended(suspended bool) {
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()
	s.cfg.Suspended = suspended
}
