package main

import "testing"

func TestSocketPathDefaultsToMountPath(t *testing.T) {
	if got := socketPath(func(string) string { return "" }); got != defaultSocket {
		t.Fatalf("got %q, want %q", got, defaultSocket)
	}
}

func TestSocketPathHonoursEnvOverride(t *testing.T) {
	env := map[string]string{"HOSTRUN_SOCKET": "/tmp/custom.sock"}
	if got := socketPath(func(k string) string { return env[k] }); got != "/tmp/custom.sock" {
		t.Fatalf("got %q, want %q", got, "/tmp/custom.sock")
	}
}
