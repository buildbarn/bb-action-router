package actionrouter

import (
	"context"
	"errors"
	"log/slog"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	re_blobstore "github.com/buildbarn/bb-remote-execution/pkg/blobstore"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	bb_digest "github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/semaphore"
)

// ImageToCASUploader pulls Docker images and uploads their contents to CAS
type ImageToCASUploader struct {
	puller     *docker.ImagePuller
	batchedCas bb_blobstore.BlobAccess
	casFlusher func(context.Context) error
	buildUser  blobstore.UnixUser
}

var imagePullDurationSeconds = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "buildbarn",
		Subsystem: "docker_ar",
		Name:      "image_pull_duration_seconds",
		Help:      "Time taken to pull and upload Docker images to CAS",
		Buckets:   prometheus.DefBuckets,
	},
	[]string{"status"},
)

// NewImageToCasUploader creates a new ImageToCASUploader.
func NewImageToCasUploader(puller *docker.ImagePuller, cas bb_blobstore.BlobAccess, buildUser blobstore.UnixUser) *ImageToCASUploader {
	// Use batched storage to improve upload performance. Parameters are tuned for a local block storage implementation
	// and the main benefit of using it is that the Put operations effectively become non-blocking.
	// Use a semaphore to limit concurrency during batch uploads
	uploadConcurrencySemaphore := semaphore.NewWeighted(50)
	// For a non-local CAS use batch size == RecommendedFindMissingDigestsCount
	batchSize := 100
	batchedCAS, casFlusher := re_blobstore.NewBatchedStoreBlobAccess(
		cas,
		bb_digest.KeyWithoutInstance,
		batchSize,
		uploadConcurrencySemaphore,
	)
	return &ImageToCASUploader{
		puller:     puller,
		batchedCas: batchedCAS,
		casFlusher: casFlusher,
		buildUser:  buildUser,
	}
}

// UploadImageToCAS pulls the Docker image and uploads all its layers to CAS,
// returning the digest of the merged root directory.
func (u *ImageToCASUploader) UploadImageToCAS(ctx context.Context, ref string, digestFunction bb_digest.Function) (*remoteexecution.Digest, error) {
	startTime := time.Now()

	digest, err := u.uploadImageToCASImpl(ctx, ref, digestFunction)

	status := "success"
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timeout"
		} else {
			status = "error"
		}
	}

	uploadDuration := time.Since(startTime)
	imagePullDurationSeconds.WithLabelValues(status).Observe(uploadDuration.Seconds())
	slog.Info("ImageToCASUploader finished uploading image", "image", ref, "status", status, "duration", uploadDuration)

	return digest, err
}

func (u *ImageToCASUploader) uploadImageToCASImpl(ctx context.Context, ref string, digestFunction bb_digest.Function) (digest *remoteexecution.Digest, retErr error) {
	// Flush batched uploads to CAS. We want this to always run as it ensures all temp files are deleted.
	defer func() {
		if err := u.casFlusher(ctx); err != nil {
			if retErr == nil {
				retErr = util.StatusWrap(err, "Failed to flush batched uploads to CAS")
			} else {
				slog.Error("casFlusher error during cleanup", "image", ref, "err", err)
			}
		}
	}()

	slog.Info("Will upload image to CAS...", "image", ref)
	img, cancel, err := u.puller.GetImageFromRef(ref)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to pull image")
	}
	defer cancel()

	layers, err := img.Layers()
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to get image layers")
	}

	var totalLayerSize int64
	for _, layer := range layers {
		// Get layer size
		size, err := layer.Size()
		if err == nil {
			totalLayerSize += size
		}
	}
	oneMb := float64(totalLayerSize) / (1024 * 1024)
	slog.Info("Image layer summary", "image", ref, "layers", len(layers), "totalLayerSizeBytes", totalLayerSize, "totalLayerSizeMB", oneMb)

	// TODO: would be nice to make this concurrent at some point. Things to consider:
	// - probably would need to split this across mulitple Pods to get more
	//   network bandwidth (both to storage and to Artifactory)
	// - would need to handle whiteout and overwrites when merging multiple layers.
	extractor := blobstore.NewCASUploadingLayerExtractor(u.batchedCas, digestFunction)
	visitor := blobstore.NewBuildUserInjectingVisitor(extractor, u.buildUser)
	for i, layer := range layers {
		slog.Debug("Processing image layer", "image", ref, "layer", i+1, "layers", len(layers))

		if err := docker.ExtractAndVisitLayerContents(ctx, layer, visitor); err != nil {
			return nil, util.StatusWrap(err, "Failed to extract layer")
		}
	}

	rootDigest, err := extractor.UploadDirectories(ctx)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to upload directories")
	}

	slog.Info("Image uploaded to CAS", "image", ref, "rootDigest", rootDigest.String())

	return rootDigest, nil
}
