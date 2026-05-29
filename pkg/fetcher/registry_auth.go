package fetcher

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/buildbarn/bb-action-router/pkg/docker"
	pb_registry_auth "github.com/buildbarn/bb-action-router/pkg/proto/configuration/registry_auth"
)

type dockerAuthConfig struct {
	Auths map[string]struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

func loadCredentialsFromFile(path string) (map[string]*docker.RegistryAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config dockerAuthConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	authConfig := make(map[string]*docker.RegistryAuth)
	for registry, creds := range config.Auths {
		authConfig[registry] = &docker.RegistryAuth{
			Username: creds.Username,
			Password: creds.Password,
		}
	}
	return authConfig, nil
}

// ParseRegistryAuth converts proto registry authentication configs into
// a map of registry hostname to credentials.
func ParseRegistryAuth(configs []*pb_registry_auth.RegistryAuthenticationConfiguration) (map[string]*docker.RegistryAuth, error) {
	result := make(map[string]*docker.RegistryAuth)
	for _, regAuth := range configs {
		switch auth := regAuth.Authentication.(type) {
		case *pb_registry_auth.RegistryAuthenticationConfiguration_Anonymous:
			if auth.Anonymous.Registry != "" {
				result[auth.Anonymous.Registry] = &docker.RegistryAuth{AnonymousToken: true}
			}
		case *pb_registry_auth.RegistryAuthenticationConfiguration_Inline:
			if auth.Inline.Registry != "" {
				result[auth.Inline.Registry] = &docker.RegistryAuth{
					Username: auth.Inline.Username,
					Password: auth.Inline.Password,
				}
			}
		case *pb_registry_auth.RegistryAuthenticationConfiguration_CredentialsJsonPath:
			fileAuthConfig, err := loadCredentialsFromFile(auth.CredentialsJsonPath)
			if err != nil {
				return nil, fmt.Errorf("failed to load credentials from %s: %w", auth.CredentialsJsonPath, err)
			}
			for registry, creds := range fileAuthConfig {
				result[registry] = creds
			}
		}
	}
	return result, nil
}
