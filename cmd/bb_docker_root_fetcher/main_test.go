package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnixSocketMode(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "fetcher.sock")
	listener, err := listenUnixSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o660); got != want {
		t.Errorf("Socket permissions are %#o, want %#o", got, want)
	}
}
