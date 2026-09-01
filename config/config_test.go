package config

import "testing"

func TestModpackInstallDefaults(t *testing.T) {
	c, err := NewAtPath("/tmp/nonexistent-rig-config.yml")
	if err != nil {
		t.Fatalf("unexpected error building config: %v", err)
	}
	if c.System.ModpackInstall.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent default = %d, want 3", c.System.ModpackInstall.MaxConcurrent)
	}
	if c.System.ModpackInstall.TimeoutMinutes != 30 {
		t.Errorf("TimeoutMinutes default = %d, want 30", c.System.ModpackInstall.TimeoutMinutes)
	}
}
