// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
//   For website terms of use, trademark policy, privacy policy and other
//   project policies see https://lfprojects.org/policies
// Copyright (c) 2019-2020, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

// TODO(ian): The build package should be refactored to make each conveyorpacker
// its own separate package. With that change, this file should be grouped with the
// OCIConveyorPacker code

package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	apexlog "github.com/apex/log"
	"github.com/apptainer/apptainer/internal/pkg/util/fs"
	sytypes "github.com/apptainer/apptainer/pkg/build/types"
	"github.com/apptainer/apptainer/pkg/sylog"
	"github.com/apptainer/apptainer/pkg/util/namespaces"
	"github.com/containers/image/v5/types"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	umocilayer "github.com/opencontainers/umoci/oci/layer"
	"github.com/opencontainers/umoci/pkg/idtools"
)

// unpackRootfs extracts all of the layers of the given image reference into the rootfs of the provided bundle
func unpackRootfs(ctx context.Context, b *sytypes.Bundle, tmpfsRef types.ImageReference, sysCtx *types.SystemContext) (err error) {
	var mapOptions umocilayer.MapOptions

	loggerLevel := sylog.GetLevel()

	// set the apex log level, for umoci
	if loggerLevel <= int(sylog.ErrorLevel) {
		// silent option
		apexlog.SetLevel(apexlog.ErrorLevel)
	} else if loggerLevel <= int(sylog.LogLevel) {
		// quiet option
		apexlog.SetLevel(apexlog.WarnLevel)
	} else if loggerLevel < int(sylog.DebugLevel) {
		// verbose option(s) or default
		apexlog.SetLevel(apexlog.InfoLevel)
	} else {
		// debug option
		apexlog.SetLevel(apexlog.DebugLevel)
	}

	// Allow unpacking as non-root
	if namespaces.IsUnprivileged() {
		sylog.Debugf("setting umoci rootless mode")
		mapOptions.Rootless = true

		uidMap, err := idtools.ParseMapping(fmt.Sprintf("0:%d:1", os.Geteuid()))
		if err != nil {
			return fmt.Errorf("error parsing uidmap: %s", err)
		}
		mapOptions.UIDMappings = append(mapOptions.UIDMappings, uidMap)

		gidMap, err := idtools.ParseMapping(fmt.Sprintf("0:%d:1", os.Getegid()))
		if err != nil {
			return fmt.Errorf("error parsing gidmap: %s", err)
		}
		mapOptions.GIDMappings = append(mapOptions.GIDMappings, gidMap)
	}

	engineExt, err := umoci.OpenLayout(b.TmpDir)
	if err != nil {
		return fmt.Errorf("error opening layout: %s", err)
	}

	// Obtain the manifest
	imageSource, err := tmpfsRef.NewImageSource(ctx, sysCtx)
	if err != nil {
		return fmt.Errorf("error creating image source: %s", err)
	}
	manifestData, mediaType, err := imageSource.GetManifest(ctx, nil)
	if err != nil {
		return fmt.Errorf("error obtaining manifest source: %s", err)
	}
	if mediaType != imgspecv1.MediaTypeImageManifest {
		return fmt.Errorf("error verifying manifest media type: %s", mediaType)
	}
	var manifest imgspecv1.Manifest
	json.Unmarshal(manifestData, &manifest)
	manifest.Config.MediaType = imgspecv1.MediaTypeImageConfig

	for idx, layerDescriptor := range manifest.Layers {
		switch mediaType := layerDescriptor.MediaType; mediaType {
		case "application/vnd.docker.image.rootfs.diff.tar.gzip":
			manifest.Layers[idx].MediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
		case "application/vnd.docker.image.rootfs.diff.tar":
			manifest.Layers[idx].MediaType = "application/vnd.oci.image.layer.v1.tar"
		}
	}

	// UnpackRootfs from umoci v0.4.2 expects a path to a non-existing directory
	os.RemoveAll(b.RootfsPath)

	// Unpack root filesystem
	unpackOptions := umocilayer.UnpackOptions{MapOptions: mapOptions}
	err = umocilayer.UnpackRootfs(ctx, engineExt, b.RootfsPath, manifest, &unpackOptions)
	if err != nil {
		return fmt.Errorf("error unpacking rootfs: %s", err)
	}

	// If the `--fix-perms` flag was used, then modify the permissions so that
	// content has owner rwX and we're done
	if b.Opts.FixPerms {
		sylog.Warningf("The --fix-perms option modifies the filesystem permissions on the resulting container.")
		sylog.Debugf("Modifying permissions for file/directory owners")
		return sytypes.FixPerms(b.RootfsPath)
	}

	// If `--fix-perms` was not used and this is a sandbox, scan for restrictive
	// perms that would stop the user doing an `rm` without a chmod first,
	// and warn if they exist
	if b.Opts.SandboxTarget {
		sylog.Debugf("Scanning for restrictive permissions")
		return checkPerms(b.RootfsPath)
	}

	// No `--fix-perms` and no sandbox... we are fine
	return err
}

// checkPerms will work through the rootfs of this bundle, and find if any
// directory does not have owner rwX - which may cause unexpected issues for a
// user trying to look through, or delete a sandbox
func checkPerms(rootfs string) (err error) {
	// This is a locally defined error we can bubble up to cancel our recursive
	// structure.
	errRestrictivePerm := errors.New("restrictive file permission found")

	err = fs.PermWalkRaiseError(rootfs, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			// If the walk function cannot access a directory at all, that's an
			// obvious restrictive permission we need to warn on
			if os.IsPermission(err) {
				sylog.Debugf("Path %q has restrictive permissions", path)
				return errRestrictivePerm
			}
			return fmt.Errorf("unable to access rootfs path %s: %s", path, err)
		}
		// Warn on any directory not `rwX` - technically other combinations may
		// be traversable / removable... but are confusing to the user vs
		// the Singularity 3.4 behavior.
		if f.Mode().IsDir() && f.Mode().Perm()&0o700 != 0o700 {
			sylog.Debugf("Path %q has restrictive permissions", path)
			return errRestrictivePerm
		}
		return nil
	})

	if errors.Is(err, errRestrictivePerm) {
		sylog.Warningf("The sandbox contain files/dirs that cannot be removed with 'rm'.")
		sylog.Warningf("Use 'chmod -R u+rwX' to set permissions that allow removal.")
		sylog.Warningf("Use the '--fix-perms' option to 'apptainer build' to modify permissions at build time.")
		// It's not an error any further up... the rootfs is still usable
		return nil
	}
	return err
}
