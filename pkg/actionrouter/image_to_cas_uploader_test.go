package actionrouter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/golang/mock/gomock"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

func TestUploadImageToCASConcurrentCancellation(t *testing.T) {
	img, err := random.Image(32, 2)
	require.NoError(t, err)
	layers, err := img.Layers()
	require.NoError(t, err)
	secondLayer, err := layers[1].Digest()
	require.NoError(t, err)

	blocked := make(chan struct{})
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+secondLayer.String()) {
			close(blocked)
			<-resume
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer release()
	ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/image:latest")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))

	ctx := context.Background()
	df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
	mockCas := mock.NewMockBlobAccess(gomock.NewController(t))
	mockCas.EXPECT().FindMissing(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, digests digest.Set) (digest.Set, error) {
		if err := ctx.Err(); err != nil {
			return digest.EmptySet, err
		}
		return digests, nil
	}).AnyTimes()
	// Both file blobs and the root directory must survive the other call's cancellation.
	mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ digest.Digest, b buffer.Buffer) error {
		b.Discard()
		return nil
	}).Times(3)
	uploader := NewImageToCasUploader(docker.NewImagePuller(nil, 0, time.Minute), mockCas, blobstore.UnixUser{})
	done := make(chan error, 1)
	go func() {
		_, err := uploader.UploadImageToCAS(ctx, ref.String(), df)
		done <- err
	}()

	// The first file is queued when extraction starts downloading the second layer.
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach the second layer")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = uploader.UploadImageToCAS(cancelled, "invalid reference", df)
	require.Error(t, err)

	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
}
