package main

import (
	"io"
	"testing"
)

func TestParseServeAcceptsAllFlags(t *testing.T) {
	got, err := parseServe([]string{"--socket", "/run/h.sock", "--workspace", "/home/u/app", "--container-workspace", "/workspaces/app"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := serveConfig{socket: "/run/h.sock", workspace: "/home/u/app", containerWorkspace: "/workspaces/app"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseServeRequiresEveryFlag(t *testing.T) {
	full := map[string]string{"--socket": "/run/h.sock", "--workspace": "/home/u/app", "--container-workspace": "/workspaces/app"}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		if _, err := parseServe(args, io.Discard); err == nil {
			t.Errorf("expected an error when %s is missing", missing)
		}
	}
}

func TestParseServeRejectsPositionalArguments(t *testing.T) {
	args := []string{"--socket", "/run/h.sock", "--workspace", "/w", "--container-workspace", "/c", "extra"}
	if _, err := parseServe(args, io.Discard); err == nil {
		t.Fatal("expected an error for an unexpected positional argument")
	}
}
