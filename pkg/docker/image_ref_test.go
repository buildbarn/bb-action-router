package docker

import (
	"testing"
)

func TestValidateImageReference(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		wantErr  bool
	}{
		{
			name:     "valid docker.io with digest",
			imageRef: "docker.io/library/ubuntu@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			wantErr:  false,
		},
		{
			name:     "invalid - using latest tag",
			imageRef: "gcr.io/project/image:latest",
			wantErr:  true,
		},
		{
			name:     "invalid - no tag or digest",
			imageRef: "gcr.io/project/image",
			wantErr:  true,
		},
		{
			name:     "invalid - empty string",
			imageRef: "",
			wantErr:  true,
		},
		{
			name:     "invalid - short digest",
			imageRef: "gcr.io/project/image@sha256:abc123",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImageReferenceIsShaDigest(tt.imageRef)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateImageReference() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetRegistryHostname(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		want     string
		wantErr  bool
	}{
		{
			name:     "gcr.io registry",
			imageRef: "gcr.io/project/image@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			want:     "gcr.io",
			wantErr:  false,
		},
		{
			name:     "custom registry with port",
			imageRef: "my-registry.com:5000/image@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			want:     "my-registry.com:5000",
			wantErr:  false,
		},
		{
			name:     "implicit docker.io",
			imageRef: "ubuntu@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			want:     "index.docker.io",
			wantErr:  false,
		},
		{
			name:     "invalid reference",
			imageRef: "not a valid reference",
			want:     "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetRegistryHostname(tt.imageRef)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetRegistryHostname() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetRegistryHostname() = %v, want %v", got, tt.want)
			}
		})
	}
}
