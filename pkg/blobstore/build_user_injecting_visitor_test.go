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

func (r *recordingVisitor) OnDirectorySeen(_ context.Context, _ string) error { return nil }

func (r *recordingVisitor) OnFileSeen(_ context.Context, path string, data io.Reader, _ int64) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	r.files[path] = string(b)
	return nil
}

func (r *recordingVisitor) OnLinkSeen(_ context.Context, _, _ string) error    { return nil }
func (r *recordingVisitor) OnSymlinkSeen(_ context.Context, _, _ string) error { return nil }
func (r *recordingVisitor) OnLayerComplete(_ context.Context) error            { return nil }

func TestBuildUserInjectingVisitor_Passwd(t *testing.T) {
	const defaultRootLine = "root:x:0:0::/tmp:/bin/sh\n"
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
			name:  "uid 0 home is rewritten to /tmp",
			input: "root:x:0:0::/root:/bin/sh\n",
			want:  defaultRootLine,
		},
		{
			name:  "uid 0 shell is preserved",
			input: "root:x:0:0::/root:/bin/zsh\n",
			want:  "root:x:0:0::/tmp:/bin/zsh\n",
		},
		{
			name:  "uid 0 under a different name",
			input: "toor:x:0:0::/root:/bin/sh\nubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
			want:  "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash\ntoor:x:0:0::/tmp:/bin/sh\n",
		},
		{
			name:  "uid 0 absent",
			input: "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n",
			want:  "ubuntu:x:1000:1000::/home/ubuntu:/bin/bash\n" + defaultRootLine,
		},
		{
			name:  "0 in gid field only is ignored",
			input: "ubuntu:x:5:0::/home/ubuntu:/bin/sh\n",
			want:  "ubuntu:x:5:0::/home/ubuntu:/bin/sh\n" + defaultRootLine,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingVisitor()
			v := NewBuildUserInjectingVisitor(rec)
			require.NoError(t, v.OnFileSeen(context.Background(), "etc/passwd", strings.NewReader(tt.input), 0o644))
			require.Equal(t, tt.want, rec.files["etc/passwd"])
		})
	}
}

func TestBuildUserInjectingVisitor_Group(t *testing.T) {
	const defaultRootLine = "root:x:0:\n"
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
			name:  "gid 0 already present",
			input: "root:x:0:\n",
			want:  defaultRootLine,
		},
		{
			name:  "gid 0 absent",
			input: "users:x:100:\n",
			want:  "users:x:100:\n" + defaultRootLine,
		},
		{
			name:  "gid 0 under a different name",
			input: "wheel:x:0:\n",
			want:  "wheel:x:0:\n",
		},
		{
			name:  "group has a user",
			input: "wheel:x:0:root,fred\n",
			want:  "wheel:x:0:root,fred\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecordingVisitor()
			v := NewBuildUserInjectingVisitor(rec)
			require.NoError(t, v.OnFileSeen(context.Background(), "etc/group", strings.NewReader(tt.input), 0o644))
			require.Equal(t, tt.want, rec.files["etc/group"])
		})
	}
}

func TestBuildUserInjectingVisitor_OtherFilesPassThrough(t *testing.T) {
	rec := newRecordingVisitor()
	v := NewBuildUserInjectingVisitor(rec)
	require.NoError(t, v.OnFileSeen(context.Background(), "etc/hosts", strings.NewReader("127.0.0.1 localhost\n"), 0o644))
	require.Equal(t, "127.0.0.1 localhost\n", rec.files["etc/hosts"])
}
