package main

import (
	"os"
	"path/filepath"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/ephemeralcas"
	"github.com/buildbarn/bb-remote-execution/pkg/cas"
	bb_cas "github.com/buildbarn/bb-storage/pkg/cas"
	"github.com/buildbarn/bb-storage/pkg/digest"
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

func TestBuildRootHostsFile(t *testing.T) {
	ctx := t.Context()
	blobs := ephemeralcas.New(t.TempDir()).CAS()
	putter := cas.NewProtoPutter(bb_cas.NewReaderPutter(blobs.ChunkStorage, blobs.ChunkMappingStorage), blobs.CDCParametersFetcher)
	digestFunction := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
	m := materializer{
		rootsDir:         t.TempDir(),
		fetchParallelism: 1,
		maxMessageSize:   1 << 20,
	}
	fetch := m.newRootFetcher(blobs)

	const imageTarget = "/usr/share/image-hosts"
	for _, tc := range []struct {
		name     string
		symlinks []*remoteexecution.SymlinkNode
	}{
		{name: "missing"},
		{name: "image symlink", symlinks: []*remoteexecution.SymlinkNode{{Name: "hosts", Target: imageTarget}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			etc, err := putter.PutProto(ctx, &remoteexecution.Directory{Symlinks: tc.symlinks}, digestFunction)
			if err != nil {
				t.Fatal(err)
			}
			root, err := putter.PutProto(ctx, &remoteexecution.Directory{
				Directories: []*remoteexecution.DirectoryNode{{Name: "etc", Digest: etc.GetProto()}},
			}, digestFunction)
			if err != nil {
				t.Fatal(err)
			}

			dir, err := m.buildRoot(ctx, fetch, root)
			if err != nil {
				t.Fatal(err)
			}
			hosts := filepath.Join(dir, "etc", "hosts")
			if len(tc.symlinks) != 0 {
				if target, err := os.Readlink(hosts); err != nil || target != imageTarget {
					t.Fatalf("Readlink(%s) = %q, %v, want %q", hosts, target, err, imageTarget)
				}
				return
			}
			contents, err := os.ReadFile(hosts)
			if err != nil {
				t.Fatal(err)
			}
			if want := "127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n"; string(contents) != want {
				t.Errorf("Contents of %s = %q, want %q", hosts, contents, want)
			}
		})
	}
}
