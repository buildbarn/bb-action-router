package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestServeDrainsConnections(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "fetcher.sock")
	listener, err := listenUnixSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &connectionHandler{}
	done := make(chan error, 1)
	go func() { done <- handler.serve(ctx, listener) }()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "HI\n" {
		t.Fatalf("greeting = %q, %v; want HI", line, err)
	}
	cancel()

	// The listener must close while the existing client is still connected.
	deadline := time.Now().Add(5 * time.Second)
	for {
		extra, err := net.DialTimeout("unix", socketPath, time.Second)
		if err != nil {
			break
		}
		extra.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepts connections during shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("serve returned before the existing client disconnected: %v", err)
	default:
	}

	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the client disconnected")
	}
}
