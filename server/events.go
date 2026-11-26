package server

import (
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/system"
)

// Defines all the possible output events for a server.
const (
	DaemonMessageEvent           = "daemon message"
	InstallOutputEvent           = "install output"
	InstallStartedEvent          = "install started"
	InstallCompletedEvent        = "install completed"
	ConsoleOutputEvent           = "console output"
	StatusEvent                  = "status"
	StatsEvent                   = "stats"
	BackupRestoreCompletedEvent  = "backup restore completed"
	BackupCompletedEvent         = "backup completed"
	TransferLogsEvent            = "transfer logs"
	TransferStatusEvent          = "transfer status"
	DeletedEvent                 = "deleted"
	ModpackInstallStatusEvent    = "modpack install status"
	ModpackInstallProgressEvent  = "modpack install progress"
	SetupApplyStatusEvent        = "setup apply status"
	FileCompressCompletedEvent   = "file compress completed"
	FileDecompressCompletedEvent = "file decompress completed"
)

// FileCompressStatus is the payload of the "file compress completed" event.
// Directory echoes the request's root exactly as received, and Error is a
// customer-safe message, empty when Successful.
type FileCompressStatus struct {
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	Successful bool   `json:"successful"`
	Error      string `json:"error"`
}

// FileDecompressStatus is the payload of the "file decompress completed"
// event. Directory echoes the request's root exactly as received, and Error is
// a customer-safe message, empty when Successful.
type FileDecompressStatus struct {
	File       string `json:"file"`
	Directory  string `json:"directory"`
	Successful bool   `json:"successful"`
	Error      string `json:"error"`
}

// Events returns the server's emitter instance.
func (s *Server) Events() *events.Bus {
	s.emitterLock.Lock()
	defer s.emitterLock.Unlock()

	if s.emitter == nil {
		s.emitter = events.NewBus()
	}

	return s.emitter
}

// Sink returns the instantiated and named sink for a server. If the sink has
// not been configured yet this function will cause a panic condition.
func (s *Server) Sink(name system.SinkName) *system.SinkPool {
	sink, ok := s.sinks[name]
	if !ok {
		s.Log().Fatalf("attempt to access nil sink: %s", name)
	}
	return sink
}

// DestroyAllSinks iterates over all of the sinks configured for the server and
// destroys their instances. Note that this will cause a panic if you attempt
// to call Server.Sink() again after. This function is only used when a server
// is being deleted from the system.
func (s *Server) DestroyAllSinks() {
	s.Log().Info("destroying all registered sinks for server instance")
	for _, sink := range s.sinks {
		sink.Destroy()
	}
}
