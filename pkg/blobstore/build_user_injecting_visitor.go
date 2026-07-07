package blobstore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/buildbarn/bb-action-router/pkg/docker"
)

// UnixUser identifies the user injected into an image's /etc/passwd and
// /etc/group.
type UnixUser struct {
	UID  int
	GID  int
	Name string
}

// BuildUserInjectingVisitor wraps a LayerContentsVisitor and ensures
// /etc/passwd and /etc/group contain an entry for the build user as layers
// are processed.
type BuildUserInjectingVisitor struct {
	inner docker.LayerContentsVisitor
	user  UnixUser
}

// NewBuildUserInjectingVisitor returns a visitor that appends an entry for
// the given build user to /etc/passwd and /etc/group when missing.
func NewBuildUserInjectingVisitor(inner docker.LayerContentsVisitor, user UnixUser) *BuildUserInjectingVisitor {
	return &BuildUserInjectingVisitor{inner: inner, user: user}
}

// OnDirectorySeen forwards to the wrapped visitor.
func (v *BuildUserInjectingVisitor) OnDirectorySeen(ctx context.Context, path string) error {
	return v.inner.OnDirectorySeen(ctx, path)
}

// OnFileSeen forwards to the wrapped visitor, while modifying file contents as needed.
func (v *BuildUserInjectingVisitor) OnFileSeen(ctx context.Context, path string, data io.Reader, mode int64) error {
	switch path {
	case "etc/passwd":
		content, err := setIDEntry(data, v.user.UID, v.user.Name, func(name, shell, ignored string) string {
			return fmt.Sprintf("%s:x:%d:%d::/tmp:%s\n", name, v.user.UID, v.user.GID, shell)
		})
		if err != nil {
			return fmt.Errorf("ensuring /etc/passwd has the build user: %w", err)
		}
		return v.inner.OnFileSeen(ctx, path, content, mode)
	case "etc/group":
		content, err := setIDEntry(data, v.user.GID, v.user.Name, func(name, ignored, members string) string {
			return fmt.Sprintf("%s:x:%d:%s\n", name, v.user.GID, members)
		})
		if err != nil {
			return fmt.Errorf("ensuring /etc/group has the build group: %w", err)
		}
		return v.inner.OnFileSeen(ctx, path, content, mode)
	default:
		return v.inner.OnFileSeen(ctx, path, data, mode)
	}
}

// OnLinkSeen forwards to the wrapped visitor.
func (v *BuildUserInjectingVisitor) OnLinkSeen(ctx context.Context, path, target string) error {
	return v.inner.OnLinkSeen(ctx, path, target)
}

// OnSymlinkSeen forwards to the wrapped visitor.
func (v *BuildUserInjectingVisitor) OnSymlinkSeen(ctx context.Context, path, target string) error {
	return v.inner.OnSymlinkSeen(ctx, path, target)
}

// OnLayerComplete forwards to the wrapped visitor.
func (v *BuildUserInjectingVisitor) OnLayerComplete(ctx context.Context) error {
	return v.inner.OnLayerComplete(ctx)
}

// setIDEntry makes sure that the entry corresponding to the given ID matches
// entryLine.
func setIDEntry(data io.Reader, id int, defaultName string, entryLine func(string, string, string) string) (io.Reader, error) {
	scanner := bufio.NewScanner(data)
	var lines []string
	name := defaultName
	shell := "/bin/sh"
	members := ""
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.SplitN(line, ":", 7)
		if len(fields) >= 3 {
			parsedID, err := strconv.Atoi(fields[2])
			if err == nil && parsedID == id {
				name = fields[0]
				if len(fields) == 7 {
					shell = fields[6]
				}
				if len(fields) > 3 {
					members = fields[3]
				}
				continue
			}
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	lines = append(lines, entryLine(name, shell, members))
	return strings.NewReader(strings.Join(lines, "\n")), nil
}
