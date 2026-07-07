package actionrouter

import (
	"testing"

	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	build_user_pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/build_user"

	"github.com/stretchr/testify/require"
)

func TestBuildUserFromProto(t *testing.T) {
	t.Run("nil yields defaults", func(t *testing.T) {
		user, err := BuildUserFromProto(nil)
		require.NoError(t, err)
		require.Equal(t, blobstore.UnixUser{UID: 0, GID: 0, Name: "root"}, user)
	})
	t.Run("values used verbatim", func(t *testing.T) {
		user, err := BuildUserFromProto(&build_user_pb.BuildUser{Uid: 2000, Gid: 3000, Name: "runner"})
		require.NoError(t, err)
		require.Equal(t, blobstore.UnixUser{UID: 2000, GID: 3000, Name: "runner"}, user)
	})
	t.Run("uid/gid used verbatim when message is present", func(t *testing.T) {
		user, err := BuildUserFromProto(&build_user_pb.BuildUser{Uid: 1000, Gid: 1000, Name: "build"})
		require.NoError(t, err)
		require.Equal(t, blobstore.UnixUser{UID: 1000, GID: 1000, Name: "build"}, user)
	})
	t.Run("empty name falls back to default for uid 0", func(t *testing.T) {
		user, err := BuildUserFromProto(&build_user_pb.BuildUser{Uid: 0, Gid: 0})
		require.NoError(t, err)
		require.Equal(t, blobstore.UnixUser{UID: 0, GID: 0, Name: "root"}, user)
	})
	t.Run("empty name is rejected for a non-zero uid", func(t *testing.T) {
		// Defaulting to "root" here would add a second entry claiming the
		// name, contradicting the image's own root:x:0:0 line.
		_, err := BuildUserFromProto(&build_user_pb.BuildUser{Uid: 1000, Gid: 1000})
		require.Error(t, err)
	})
}
