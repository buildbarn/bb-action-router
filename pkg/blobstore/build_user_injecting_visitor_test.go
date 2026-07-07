package blobstore

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingVisitor is a LayerContentsVisitor that captures the
// bytes passed to OnFileSeen so tests can assert on the post-
// injection content.
type recordingVisitor struct {
	files map[string]string
}

func newRecordingVisitor() *recordingVisitor {
	return &recordingVisitor{files: map[string]string{}}
}

func (recordingVisitor) OnDirectorySeen(_ context.Context, _ string) error { return nil }

func (r *recordingVisitor) OnFileSeen(_ context.Context, path string, data io.Reader, _ int64) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	r.files[path] = string(b)
	return nil
}

func (recordingVisitor) OnLinkSeen(_ context.Context, _, _ string) error    { return nil }
func (recordingVisitor) OnSymlinkSeen(_ context.Context, _, _ string) error { return nil }
func (recordingVisitor) OnLayerComplete(_ context.Context) error            { return nil }

func TestBuildUserInjectingVisitor_Passwd(t *testing.T) {
	const defaultRootLine = "build:x:1000:1000::/tmp:/bin/sh\n"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty file",
			input: "",
			want:  defaultRootLine,
		},
		{
			name:  "uid 1000 home is rewritten to /tmp",
			input: "build:x:1000:1000::/root:/bin/sh\n",
			want:  defaultRootLine,
		},
		{
			name:  "uid 1000 shell is preserved",
			input: "build:x:1000:1000::/root:/bin/zsh\n",
			want:  "build:x:1000:1000::/tmp:/bin/zsh\n",
		},
		{
			name:  "uid 1000 under a different name",
			input: "root:x:0:0::/root:/bin/sh\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
			want:  "root:x:0:0::/root:/bin/sh\nubuntu:x:1000:1000::/tmp:/bin/bash\n",
		},
		{
			name:  "uid 1000 absent",
			input: "ubuntu:x:1001:1001::/home/ubuntu:/bin/bash\n",
			want:  "ubuntu:x:1001:1001::/home/ubuntu:/bin/bash\n" + defaultRootLine,
		},
		{
			name:  "1000 in gid field only is ignored",
			input: "ubuntu:x:5:1000::/home/ubuntu:/bin/sh\n",
			want:  "ubuntu:x:5:1000::/home/ubuntu:/bin/sh\n" + defaultRootLine,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingVisitor()
			v := NewBuildUserInjectingVisitor(rec, UnixUser{UID: 1000, GID: 1000, Name: "build"})
			require.NoError(t, v.OnFileSeen(context.Background(), "etc/passwd", strings.NewReader(tt.input), 0o644))
			require.Equal(t, tt.want, rec.files["etc/passwd"])
		})
	}
}

func TestBuildUserInjectingVisitor_Group(t *testing.T) {
	const defaultRootLine = "build:x:1000:\n"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty file",
			input: "",
			want:  defaultRootLine,
		},
		{
			name:  "gid 1000 already present",
			input: "build:x:1000:\n",
			want:  defaultRootLine,
		},
		{
			name:  "gid 1000 absent",
			input: "users:x:100:\n",
			want:  "users:x:100:\n" + defaultRootLine,
		},
		{
			name:  "gid 1000 under a different name",
			input: "wheel:x:1000:\n",
			want:  "wheel:x:1000:\n",
		},
		{
			name:  "group has a user",
			input: "wheel:x:1000:root,fred\n",
			want:  "wheel:x:1000:root,fred\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingVisitor()
			v := NewBuildUserInjectingVisitor(rec, UnixUser{UID: 1000, GID: 1000, Name: "build"})
			require.NoError(t, v.OnFileSeen(context.Background(), "etc/group", strings.NewReader(tt.input), 0o644))
			require.Equal(t, tt.want, rec.files["etc/group"])
		})
	}
}

func TestBuildUserInjectingVisitor_OtherFilesPassThrough(t *testing.T) {
	rec := newRecordingVisitor()
	v := NewBuildUserInjectingVisitor(rec, UnixUser{UID: 1000, GID: 1000, Name: "build"})
	require.NoError(t, v.OnFileSeen(context.Background(), "etc/hosts", strings.NewReader("127.0.0.1 localhost\n"), 0o644))
	require.Equal(t, "127.0.0.1 localhost\n", rec.files["etc/hosts"])
}
