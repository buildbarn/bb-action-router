package actionrouter

import (
	"context"
	"errors"
	"log"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	re_blobstore "github.com/buildbarn/bb-remote-execution/pkg/blobstore"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	bb_digest "github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	"golang.org/x/sync/semaphore"
)

// ImageToCASUploader pulls Docker images and uploads their contents to CAS
type ImageToCASUploader struct {
	puller     *docker.ImagePuller
	batchedCas bb_blobstore.BlobAccess
	casFlusher func(context.Context) error
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
func NewImageToCasUploader(puller *docker.ImagePuller, cas bb_blobstore.BlobAccess) *ImageToCASUploader {
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
		uploadConcurrencySemaphore)
	return &ImageToCASUploader{
		puller:     puller,
		batchedCas: batchedCAS,
		casFlusher: casFlusher,
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

	uploadDurationSeconds := time.Since(startTime).Seconds()
	imagePullDurationSeconds.WithLabelValues(status).Observe(uploadDurationSeconds)
	log.Printf("ImageToCASUploader %s when uploading image %s in %.2f seconds.\n", status, ref, uploadDurationSeconds)

	return digest, err
}

func (u *ImageToCASUploader) uploadImageToCASImpl(ctx context.Context, ref string, digestFunction bb_digest.Function) (digest *remoteexecution.Digest, retErr error) {
	// Flush batched uploads to CAS. We want this to always run as it ensures all temp files are deleted.
	defer func() {
		if err := u.casFlusher(ctx); err != nil {
			if retErr == nil {
				retErr = util.StatusWrap(err, "Failed to flush batched uploads to CAS")
			} else {
				log.Printf("casFlusher error during cleanup of %s: %v", ref, err)
			}
		}
	}()

	log.Printf("Will upload %s to CAS...", ref)
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
	log.Printf("Image %s has %d layers. Total layer size %d bytes (%.2f MB)", ref, len(layers), totalLayerSize, oneMb)

	// TODO: would be nice to make this concurrent at some point. Things to consider:
	// - probably would need to split this across mulitple Pods to get more
	//   network bandwidth (both to storage and to Artifactory)
	// - would need to handle whiteout and overwrites when merging multiple layers.
	extractor := blobstore.NewCASUploadingLayerExtractor(u.batchedCas, digestFunction)
	visitor := blobstore.NewBuildUserInjectingVisitor(extractor)
	for i, layer := range layers {
		log.Printf("Image %s, processing layer %d...", ref, i+1)

		if err := docker.ExtractAndVisitLayerContents(ctx, layer, visitor); err != nil {
			return nil, util.StatusWrap(err, "Failed to extract layer")
		}
	}

	rootDigest, err := extractor.UploadDirectories(ctx)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to upload directories")
	}

	log.Printf("Image %s uploaded to CAS: root digest %s.", ref, rootDigest.String())

	return rootDigest, nil
}
