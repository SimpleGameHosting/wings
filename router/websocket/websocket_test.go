package websocket

import (
	"testing"

	"github.com/pterodactyl/wings/server"
)

func TestEventPermission(t *testing.T) {
	cases := []struct {
		event      Event
		permission string
		gated      bool
	}{
		{server.FileCompressCompletedEvent, "file.read", true},
		{server.FileDecompressCompletedEvent, "file.read", true},
		{server.BackupCompletedEvent + ":abc", "backup.read", true},
		{server.InstallOutputEvent, "admin.websocket.install", true},
		{server.TransferLogsEvent, "admin.websocket.transfer", true},
		{server.ConsoleOutputEvent, "", false},
	}

	for _, testCase := range cases {
		permission, gated := eventPermission(testCase.event)
		if permission != testCase.permission || gated != testCase.gated {
			t.Errorf("eventPermission(%q) = (%q, %v), want (%q, %v)", testCase.event, permission, gated, testCase.permission, testCase.gated)
		}
	}
}
