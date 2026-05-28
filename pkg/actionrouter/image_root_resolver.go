package actionrouter

import (
	"context"
	"slices"

	pb "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_action_router"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ImageRootResolver resolves container images to input root directory digests.
type ImageRootResolver interface {
	ResolveImageRoot(ctx context.Context, digestFunction digest.Function, image string) (digest.Digest, error)
}

type registryImageRootResolver struct {
	configuration             *pb.ImageRootResolverConfiguration
	contentAddressableStorage blobstore.BlobAccess
}

// TODO: cache the image roots
// map[cachedImageRootKey].digest

func newImageRootResolverFromConfiguration(configuration *pb.ImageRootResolverConfiguration, contentAddressableStorage blobstore.BlobAccess) (ImageRootResolver, error) {
	if configuration == nil {
		return nil, status.Error(codes.InvalidArgument, "No image root resolver configuration provided")
	}
	return registryImageRootResolver{
		configuration:             configuration,
		contentAddressableStorage: contentAddressableStorage,
	}, nil
}

func (r registryImageRootResolver) ResolveImageRoot(ctx context.Context, digestFunction digest.Function, image string) (digest.Digest, error) {
	imageReference, err := name.NewDigest(image, name.StrictValidation)
	if err != nil {
		return digest.BadDigest, status.Errorf(codes.InvalidArgument, "Invalid container image reference %#v: %s", image, err)
	}
	if err := r.validateImageReference(imageReference); err != nil {
		return digest.BadDigest, err
	}

	imageDescriptor, err := remote.Get(imageReference, remote.WithContext(ctx))
	if err != nil {
		return digest.BadDigest, util.StatusWrap(err, "Failed to fetch container image manifest")
	}
	// The configuration has no platform selector, so an index or manifest list
	// does not tell us what to pick.
	switch imageDescriptor.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		return digest.BadDigest, status.Errorf(codes.InvalidArgument, "Container image reference %#v resolves to media type %#v, while an image manifest was expected", image, imageDescriptor.MediaType)
	}
	containerImage, err := imageDescriptor.Image()
	if err != nil {
		return digest.BadDigest, util.StatusWrap(err, "Failed to obtain container image")
	}

	imageRootReader := mutate.Extract(containerImage)
	imageRootDirectory, err := readImageRootTar(ctx, r.contentAddressableStorage, digestFunction, imageRootReader)
	errClose := imageRootReader.Close()
	if err != nil {
		return digest.BadDigest, err
	}
	if errClose != nil {
		return digest.BadDigest, util.StatusWrap(errClose, "Failed to close image root tar")
	}

	imageRootDigest, err := imageRootDirectory.uploadImageRootDirectory(ctx, r.contentAddressableStorage, digestFunction)
	if err != nil {
		return digest.BadDigest, util.StatusWrap(err, "Failed to upload image root directory")
	}
	return imageRootDigest, nil
}

func (r registryImageRootResolver) validateImageReference(imageReference name.Digest) error {
	registry := imageReference.Context().RegistryStr()
	repository := imageReference.Context().RepositoryStr()

	registryMatched := false
	for _, allowedRegistry := range r.configuration.AllowedRegistries {
		if allowedRegistry.Registry != registry {
			continue
		}
		registryMatched = true
		if slices.Contains(allowedRegistry.Repositories, repository) {
			return nil
		}
	}
	if registryMatched {
		return status.Errorf(codes.PermissionDenied, "Repository %#v is not allowed for registry %#v", repository, registry)
	}
	return status.Errorf(codes.PermissionDenied, "Registry %#v is not allowed", registry)
}
