/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package imgutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/transfer"
	transferimage "github.com/containerd/containerd/v2/core/transfer/image"
	"github.com/containerd/containerd/v2/core/transfer/local"
	"github.com/containerd/containerd/v2/core/transfer/registry"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/imgcrypt/v2"
	"github.com/containerd/imgcrypt/v2/images/encryption"
	"github.com/containerd/log"
	"github.com/containerd/platforms"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/errutil"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/dockerconfigresolver"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/ipfs"
	"github.com/containerd/nerdctl/v2/pkg/platformutil"
	"github.com/containerd/nerdctl/v2/pkg/referenceutil"
	"github.com/containerd/nerdctl/v2/pkg/transferutil"
)

func prepareImageStore(ctx context.Context, parsedReference *referenceutil.ImageReference, options types.ImagePullOptions) (*transferimage.Store, error) {
	var storeOpts []transferimage.StoreOpt
	if len(options.OCISpecPlatform) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(options.OCISpecPlatform...))
	}

	unpackEnabled := len(options.OCISpecPlatform) == 1
	if options.Unpack != nil {
		unpackEnabled = *options.Unpack
		if unpackEnabled && len(options.OCISpecPlatform) != 1 {
			return nil, fmt.Errorf("unpacking requires a single platform to be specified (e.g., --platform=amd64)")
		}
	}

	if unpackEnabled {
		platform := options.OCISpecPlatform[0]
		snapshotter := options.GOptions.Snapshotter
		storeOpts = append(storeOpts, transferimage.WithUnpack(platform, snapshotter))
	}

	return transferimage.NewStore(parsedReference.String(), storeOpts...), nil
}

func createOCIRegistry(ctx context.Context, parsedReference *referenceutil.ImageReference, gOptions types.GlobalCommandOptions, plainHTTP bool) (*registry.OCIRegistry, func(), error) {
	ch, err := dockerconfigresolver.NewCredentialHelper(parsedReference.Domain)
	if err != nil {
		return nil, nil, err
	}

	opts := []registry.Opt{
		registry.WithCredentials(ch),
	}

	var tmpHostsDir string
	cleanup := func() {
		if tmpHostsDir != "" {
			os.RemoveAll(tmpHostsDir)
		}
	}

	// If insecure-registry is set, create a temporary hosts.toml with skip_verify
	if gOptions.InsecureRegistry {
		tmpHostsDir, err = dockerconfigresolver.CreateTmpHostsConfig(parsedReference.Domain, true)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to create temporary hosts.toml for %q, continuing without it", parsedReference.Domain)
		} else if tmpHostsDir != "" {
			opts = append(opts, registry.WithHostDir(tmpHostsDir))
		}
	} else if len(gOptions.HostsDir) > 0 {
		opts = append(opts, registry.WithHostDir(gOptions.HostsDir[0]))
	}

	if isLocalHost, err := docker.MatchLocalhost(parsedReference.Domain); err != nil {
		cleanup()
		return nil, nil, err
	} else if isLocalHost || plainHTTP {
		opts = append(opts, registry.WithDefaultScheme("http"))
	}

	reg, err := registry.NewOCIRegistry(ctx, parsedReference.String(), opts...)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	return reg, cleanup, nil
}

func PullImageWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, rawRef string, options types.ImagePullOptions) (*EnsuredImage, error) {
	store, err := prepareImageStore(ctx, parsedReference, options)
	if err != nil {
		return nil, err
	}

	progressWriter := options.Stderr
	if options.ProgressOutputToStdout {
		progressWriter = options.Stdout
	}

	var fetcher interface{}
	var cleanup func()
	var useLocalTransfer bool

	// Check if this is an IPFS/IPNS reference
	if parsedReference.Protocol == referenceutil.IPFSProtocol ||
		parsedReference.Protocol == referenceutil.IPNSProtocol {
		// Use IPFS Registry
		ipfsReg, err := ipfs.NewRegistry(
			ctx,
			parsedReference.Path,
			string(parsedReference.Protocol),
			options.IPFSAddress,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create IPFS registry: %w", err)
		}
		fetcher = ipfsReg
		cleanup = func() {}     // IPFS doesn't need cleanup
		useLocalTransfer = true // IPFS registry is not registered in containerd, use local transfer
	} else {
		// Use standard OCI Registry
		var err error
		fetcher, cleanup, err = createOCIRegistry(ctx, parsedReference, options.GOptions, false)
		if err != nil {
			return nil, err
		}
		useLocalTransfer = false
	}
	defer cleanup()

	var transferErr error
	if useLocalTransfer {
		// Prepare decryption options for unpacking encrypted images
		var applyOpts []diff.ApplyOpt
		imgcryptPayload := imgcrypt.Payload{}
		imgcryptUnpackOpt := encryption.WithDecryptedUnpack(&imgcryptPayload)
		applyOpts = append(applyOpts, imgcryptUnpackOpt)

		// Use local transfer service for IPFS (no gRPC serialization needed)
		transferErr = doLocalTransfer(ctx, client, fetcher, store, options.Quiet, progressWriter, options.GOptions.Snapshotter, options.OCISpecPlatform, applyOpts)
	} else {
		// Use standard transfer API for OCI registries
		transferErr = doTransfer(ctx, client, fetcher, store, options.Quiet, progressWriter)
	}

	if transferErr != nil && (errors.Is(transferErr, http.ErrSchemeMismatch) || errutil.IsErrConnectionRefused(transferErr) || errutil.IsErrHTTPResponseToHTTPSClient(transferErr) || errutil.IsErrTLSHandshakeFailure(transferErr)) {
		if options.GOptions.InsecureRegistry {
			log.G(ctx).WithError(transferErr).Warnf("server %q does not seem to support HTTPS, falling back to plain HTTP", parsedReference.Domain)
			fetcher, cleanup2, err := createOCIRegistry(ctx, parsedReference, options.GOptions, true)
			if err != nil {
				return nil, err
			}
			defer cleanup2()
			transferErr = doTransfer(ctx, client, fetcher, store, options.Quiet, progressWriter)
		}
	}

	if transferErr != nil {
		return nil, transferErr
	}

	imageStore := client.ImageService()
	stored, err := store.Get(ctx, imageStore)
	if err != nil {
		return nil, err
	}

	plMatch := platformutil.NewMatchComparerFromOCISpecPlatformSlice(options.OCISpecPlatform)
	containerdImage := containerd.NewImageWithPlatform(client, stored, plMatch)
	imgConfig, err := getImageConfig(ctx, containerdImage)
	if err != nil {
		return nil, err
	}

	snapshotter := options.GOptions.Snapshotter
	snOpt := getSnapshotterOpts(snapshotter)

	return &EnsuredImage{
		Ref:         rawRef,
		Image:       containerdImage,
		ImageConfig: *imgConfig,
		Snapshotter: snapshotter,
		Remote:      snOpt.isRemote(),
	}, nil
}

func preparePushStore(pushRef string, options types.ImagePushOptions) (*transferimage.Store, error) {
	platformsSlice, err := platformutil.NewOCISpecPlatformSlice(options.AllPlatforms, options.Platforms)
	if err != nil {
		return nil, err
	}

	storeOpts := []transferimage.StoreOpt{}
	if len(platformsSlice) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(platformsSlice...))
	}

	return transferimage.NewStore(pushRef, storeOpts...), nil
}

func PushImageWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, pushRef, rawRef string, options types.ImagePushOptions) error {
	source, err := preparePushStore(pushRef, options)
	if err != nil {
		return err
	}

	progressWriter := io.Discard
	if options.Stdout != nil {
		progressWriter = options.Stdout
	}

	pusher, cleanup, err := createOCIRegistry(ctx, parsedReference, options.GOptions, false)
	if err != nil {
		return err
	}
	defer cleanup()

	transferErr := doTransfer(ctx, client, source, pusher, options.Quiet, progressWriter)

	if transferErr != nil && (errors.Is(transferErr, http.ErrSchemeMismatch) || errutil.IsErrConnectionRefused(transferErr) || errutil.IsErrHTTPResponseToHTTPSClient(transferErr) || errutil.IsErrTLSHandshakeFailure(transferErr)) {
		if options.GOptions.InsecureRegistry {
			log.G(ctx).WithError(transferErr).Warnf("server %q does not seem to support HTTPS, falling back to plain HTTP", parsedReference.Domain)
			pusher, cleanup2, err := createOCIRegistry(ctx, parsedReference, options.GOptions, true)
			if err != nil {
				return err
			}
			defer cleanup2()
			transferErr = doTransfer(ctx, client, source, pusher, options.Quiet, progressWriter)
		}
	}

	if transferErr != nil {
		log.G(ctx).WithError(transferErr).Errorf("server %q does not seem to support HTTPS", parsedReference.Domain)
		log.G(ctx).Info("Hint: you may want to try --insecure-registry to allow plain HTTP (if you are in a trusted network)")
		return transferErr
	}

	return nil
}

func doTransfer(ctx context.Context, client *containerd.Client, src, dst interface{}, quiet bool, progressWriter io.Writer) error {
	opts := make([]transfer.Opt, 0, 1)
	if !quiet {
		pf, done := transferutil.ProgressHandler(ctx, progressWriter)
		defer done()
		opts = append(opts, transfer.WithProgress(pf))
	}
	return client.Transfer(ctx, src, dst, opts...)
}

// doLocalTransfer performs transfer using local transfer service instead of gRPC proxy.
// This is used for IPFS registry which is not registered in containerd's type system.
func doLocalTransfer(ctx context.Context, client *containerd.Client, src, dst interface{}, quiet bool, progressWriter io.Writer, snapshotter string, platforms []ocispec.Platform, applyOpts []diff.ApplyOpt) error {
	opts := make([]transfer.Opt, 0, 1)
	if !quiet {
		pf, done := transferutil.ProgressHandler(ctx, progressWriter)
		defer done()
		opts = append(opts, transfer.WithProgress(pf))
	}

	// Create local transfer service with unpack configuration
	config := local.TransferConfig{
		MaxConcurrentDownloads:      3,
		MaxConcurrentUploadedLayers: 3,
	}

	// Configure unpack platforms if needed
	if iu, ok := dst.(transfer.ImageUnpacker); ok {
		unpacks := iu.UnpackPlatforms()
		if len(unpacks) > 0 {
			// Build supported unpack platforms based on what the image store wants to unpack
			// Extract platforms from UnpackConfiguration
			var unpackPlatforms []ocispec.Platform
			for _, uc := range unpacks {
				unpackPlatforms = append(unpackPlatforms, uc.Platform)
			}
			config.UnpackPlatforms = buildUnpackPlatforms(ctx, client, snapshotter, unpackPlatforms, applyOpts)
		}
	}

	transferService := local.NewTransferService(
		client.ContentStore(),
		client.ImageService(),
		config,
	)

	return transferService.Transfer(ctx, src, dst, opts...)
}

// buildUnpackPlatforms creates unpack platform configurations for local transfer service.
// This is needed to support unpacking images with the local transfer service.
func buildUnpackPlatforms(ctx context.Context, client *containerd.Client, snapshotter string, ociPlatforms []ocispec.Platform, applyOpts []diff.ApplyOpt) []unpack.Platform {
	if len(ociPlatforms) == 0 {
		return nil
	}

	var unpackPlatforms []unpack.Platform
	for _, p := range ociPlatforms {
		// Get snapshotter service
		snapshotService := client.SnapshotService(snapshotter)
		if snapshotService == nil {
			log.G(ctx).Warnf("snapshotter %q not found, skipping unpack for platform %s", snapshotter, platforms.Format(p))
			continue
		}

		// Get diff service (applier)
		diffService := client.DiffService()
		if diffService == nil {
			log.G(ctx).Warn("diff service not found, skipping unpack")
			continue
		}

		unpackPlatforms = append(unpackPlatforms, unpack.Platform{
			Platform:       platforms.OnlyStrict(p),
			SnapshotterKey: snapshotter,
			Snapshotter:    snapshotService,
			Applier:        diffService,
			ApplyOpts:      applyOpts,
		})
	}

	return unpackPlatforms
}
