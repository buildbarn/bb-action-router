package docker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestImagePuller_validateImageSize(t *testing.T) {
	numLayers := 3

	tests := []struct {
		name        string
		layerSize   int64
		maxSize     int64
		wantErr     bool
		errContains string
	}{
		{
			name:      "image within size limit",
			layerSize: 10,
			maxSize:   1000,
			wantErr:   false,
		},
		{
			name:        "image exceeds size limit",
			layerSize:   100,
			maxSize:     200,
			wantErr:     true,
			errContains: "exceeds maximum allowed size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			puller := NewImagePuller(nil, tt.maxSize, 0)
			img, err := random.Image(tt.layerSize, int64(numLayers))
			if err != nil {
				t.Fatalf("failed to create fake image: %v", err)
			}

			err = puller.validateImageSize(img)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateImageSize() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContains != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("validateImageSize() error = %v, want error containing %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestImagePuller_GetImageFromRefCancel(t *testing.T) {
	manifest, err := empty.Image.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		_, _ = w.Write(manifest)
	}))
	defer server.Close()

	for _, timeout := range []time.Duration{0, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			puller := NewImagePuller(nil, 0, timeout)
			_, cancel, err := puller.GetImageFromRef(strings.TrimPrefix(server.URL, "http://") + "/image:latest")
			if err != nil {
				t.Fatal(err)
			}
			if cancel == nil {
				t.Fatal("successful pull returned a nil cancel function")
			}
			cancel()
		})
	}
}
