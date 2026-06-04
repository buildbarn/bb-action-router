package docker

import (
	"context"
	"time"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ImagePuller pulls Docker images from container registries.
type ImagePuller struct {
	authConfig        map[string]*RegistryAuth
	maxImageSizeBytes int64
	pullTimeout       time.Duration
}

// RegistryAuth holds authentication credentials for a Docker registry.
type RegistryAuth struct {
	Username       string
	Password       string
	AnonymousToken bool
}

// NewImagePuller creates a new ImagePuller with the specified configuration.
func NewImagePuller(authConfig map[string]*RegistryAuth, maxImageSizeBytes int64, pullTimeout time.Duration) *ImagePuller {
	return &ImagePuller{
		authConfig:        authConfig,
		maxImageSizeBytes: maxImageSizeBytes,
		pullTimeout:       pullTimeout,
	}
}

// GetImageFromRef pulls a Docker image by its reference (tag or digest).
// The returned CancelFunc must be called after we're done using the image.
func (p *ImagePuller) GetImageFromRef(imageRef string) (v1.Image, context.CancelFunc, error) {
	// Create context with timeout from background, not from parent context
	// This ensures layer downloads aren't canceled if the action context is canceled
	ctx := context.Background()
	var cancel func()

	if p.pullTimeout > 0 {
		// We DON'T defer cancel() here because the layer objects need this context
		// to remain valid when downloading blobs later
		ctx, cancel = context.WithTimeout(ctx, p.pullTimeout)
	}

	img, err := p.getImageFromRefImpl(ctx, imageRef)
	if err != nil && cancel != nil {
		cancel()
		cancel = nil
	}

	return img, cancel, err
}

func (p *ImagePuller) getImageFromRefImpl(ctx context.Context, imageRef string) (v1.Image, error) {
	// Parse image reference
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to parse image reference")
	}

	auth, err := p.getAuthForRegistry(ctx, ref.Context().RegistryStr())
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to get authentication")
	}

	desc, err := remote.Get(ref, remote.WithAuth(auth), remote.WithContext(ctx))
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to fetch image descriptor")
	}

	img, err := desc.Image()
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to get image from descriptor")
	}

	if err := p.validateImageSize(img); err != nil {
		return nil, err
	}

	return img, nil
}

// validateImageSize checks if the total size of all layers exceeds the maximum.
func (p *ImagePuller) validateImageSize(img v1.Image) error {
	if p.maxImageSizeBytes <= 0 {
		return nil
	}

	manifest, err := img.Manifest()
	if err != nil {
		return util.StatusWrap(err, "Failed to get image manifest")
	}

	var totalSize int64
	for _, layer := range manifest.Layers {
		totalSize += layer.Size
	}

	if totalSize > p.maxImageSizeBytes {
		oneMB := float64(1024 * 1024)
		return status.Errorf(
			codes.InvalidArgument,
			"Image size %.2f MB exceeds maximum allowed size %.2f MB",
			float64(totalSize)/oneMB,
			float64(p.maxImageSizeBytes)/oneMB,
		)
	}

	return nil
}

// getAuthForRegistry returns the authenticator to use for the given registry.
func (p *ImagePuller) getAuthForRegistry(ctx context.Context, registryHostname string) (authn.Authenticator, error) {
	auth, ok := p.authConfig[registryHostname]
	if !ok {
		return authn.Anonymous, nil
	}

	if auth.AnonymousToken {
		return authn.Anonymous, nil
	}

	return &authn.Basic{
		Username: auth.Username,
		Password: auth.Password,
	}, nil
}
