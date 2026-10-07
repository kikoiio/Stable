package main

import (
	"strings"
	"testing"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
)

func TestRemoteCLIRejectsUnknownActionAndFlags(t *testing.T) {
	if err := runRemote([]string{"restart"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "unknown remote command") {
		t.Fatalf("unknown action error = %v", err)
	}
	if err := runRemote([]string{"pair", "--listen", "127.0.0.1:8765"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("pair flag error = %v", err)
	}
	if err := runRemote([]string{"status", "unexpected"}, appconfig.AppConfig{}, paths.Paths{}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestRemoteStatusIsStoppedWhenRuntimeIsDown(t *testing.T) {
	if err := runRemote([]string{"status"}, appconfig.AppConfig{}, paths.Paths{}); err != nil {
		t.Fatalf("status with no runtime = %v", err)
	}
	if err := runRemote([]string{"down"}, appconfig.AppConfig{}, paths.Paths{}); err != nil {
		t.Fatalf("down with no runtime = %v", err)
	}
}
