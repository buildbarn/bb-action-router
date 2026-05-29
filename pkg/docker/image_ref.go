package docker

import (
	"fmt"
	"regexp"

	"github.com/google/go-containerregistry/pkg/name"
)

const (
	// digestPattern matches SHA256 digests at the end of image references
	digestPattern = `@sha256:[a-f0-9]{64}$`
)

var digestRegex = regexp.MustCompile(digestPattern)

// ValidateImageReferenceIsShaDigest validates that an image reference uses
// SHA256 digest format.
func ValidateImageReferenceIsShaDigest(imageRef string) error {
	if imageRef == "" {
		return fmt.Errorf("image reference cannot be empty")
	}

	_, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}

	if !digestRegex.MatchString(imageRef) {
		return fmt.Errorf("must use SHA256 digest format (e.g., registry/image@sha256:...), got: %s", imageRef)
	}

	return nil
}

// GetRegistryHostname extracts the registry hostname from an image reference.
// For example: "gcr.io/project/image@sha256:abc..." returns "gcr.io"
func GetRegistryHostname(imageRef string) (string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("invalid image reference: %w", err)
	}
	return ref.Context().RegistryStr(), nil
}
