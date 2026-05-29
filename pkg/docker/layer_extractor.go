package docker

import (
	"archive/tar"
	"context"
	"io"

	"github.com/buildbarn/bb-storage/pkg/util"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// LayerContentsVisitor is called for each entry (directory, file, link, symlink) found in a Docker layer.
type LayerContentsVisitor interface {
	OnDirectorySeen(ctx context.Context, path string) error
	OnFileSeen(ctx context.Context, path string, data io.Reader, mode int64) error
	OnLinkSeen(ctx context.Context, path, target string) error
	OnSymlinkSeen(ctx context.Context, path, target string) error
	OnLayerComplete(ctx context.Context) error
}

// ExtractAndVisitLayerContents uncompresses the layer and traverses the tarball
// contents calling the supplied visitor on each entry.
func ExtractAndVisitLayerContents(ctx context.Context, layer v1.Layer, lcv LayerContentsVisitor) error {
	uncompressed, err := layer.Uncompressed()
	if err != nil {
		return util.StatusWrap(err, "Failed to uncompress layer")
	}
	defer uncompressed.Close()

	if err := processTar(ctx, uncompressed, lcv); err != nil {
		return util.StatusWrapf(err, "Failed to process layer")
	}

	return lcv.OnLayerComplete(ctx)
}

func processTar(ctx context.Context, r io.Reader, lcv LayerContentsVisitor) error {
	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return util.StatusWrap(err, "Failed to read tar header")
		}
		if err = ctx.Err(); err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			err = lcv.OnDirectorySeen(ctx, header.Name)
			if err != nil {
				return err
			}

		case tar.TypeReg:
			err = lcv.OnFileSeen(ctx, header.Name, tr, header.Mode)
			if err != nil {
				return err
			}

		case tar.TypeSymlink:
			err = lcv.OnSymlinkSeen(ctx, header.Name, header.Linkname)
			if err != nil {
				return err
			}

		case tar.TypeLink:
			err = lcv.OnLinkSeen(ctx, header.Name, header.Linkname)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
