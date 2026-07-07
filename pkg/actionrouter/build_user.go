package actionrouter

import (
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	build_user_pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/build_user"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Default build user, used when the configuration omits the build_user
// message entirely. Matches bb_chroot_helper's --build-user default (the
// in-namespace root user, mapped to the host-side unprivileged user).
var defaultBuildUser = blobstore.UnixUser{UID: 0, GID: 0, Name: "root"}

// BuildUserFromProto resolves a BuildUser configuration message into a
// UnixUser, applying defaults. A nil message yields the default build user.
// When the message is present its uid/gid are used verbatim; the name may only
// be omitted for uid 0, where it defaults to "root".
func BuildUserFromProto(m *build_user_pb.BuildUser) (blobstore.UnixUser, error) {
	if m == nil {
		return defaultBuildUser, nil
	}
	user := blobstore.UnixUser{UID: int(m.Uid), GID: int(m.Gid), Name: m.Name}
	if user.Name == "" {
		if user.UID != 0 {
			return blobstore.UnixUser{}, status.Errorf(codes.InvalidArgument, "build_user.name must be set for uid %d", user.UID)
		}
		user.Name = defaultBuildUser.Name
	}
	return user, nil
}
