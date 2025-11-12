//go:build !no_ipfs

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

package ipfs

import (
	"context"
	"encoding/json"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/transfer"
	transferimage "github.com/containerd/containerd/v2/core/transfer/image"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/imgutil"
	"github.com/containerd/nerdctl/v2/pkg/platformutil"
	"github.com/containerd/nerdctl/v2/pkg/transferutil"
)

// PullImageWithTransfer pulls an image from IPFS using the Transfer Service.
func PullImageWithTransfer(ctx context.Context, client *containerd.Client, scheme, ref, ipfsPath string, options types.ImagePullOptions) (*imgutil.EnsuredImage, error) {
	// Create IPFS source
	source, err := NewIPFSSource(ctx, scheme, ref, ipfsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create IPFS source: %w", err)
	}

	// Build store options
	storeOpts, _, err := buildTransferStoreOptions(ctx, ref, options)
	if err != nil {
		return nil, err
	}

	// Create image store destination
	dest := transferimage.NewStore(ref, storeOpts...)

	// Prepare transfer options
	transferOpts := []transfer.Opt{}
	if !options.Quiet {
		progressWriter := options.Stderr
		if options.ProgressOutputToStdout {
			progressWriter = options.Stdout
		}
		pf, done := transferutil.ProgressHandler(ctx, progressWriter)
		defer done()
		transferOpts = append(transferOpts, transfer.WithProgress(pf))
	}

	// Execute transfer
	if err := client.Transfer(ctx, source, dest, transferOpts...); err != nil {
		return nil, fmt.Errorf("failed to transfer image from IPFS: %w", err)
	}

	// Get the stored image
	imageStore := client.ImageService()
	stored, err := dest.Get(ctx, imageStore)
	if err != nil {
		return nil, fmt.Errorf("failed to get stored image: %w", err)
	}

	// Build EnsuredImage result
	plMatch := platformutil.NewMatchComparerFromOCISpecPlatformSlice(options.OCISpecPlatform)
	containerdImage := containerd.NewImageWithPlatform(client, stored, plMatch)
	
	// Get image config
	desc, err := containerdImage.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get image config descriptor: %w", err)
	}
	
	var imgConfig ocispec.ImageConfig
	switch desc.MediaType {
	case ocispec.MediaTypeImageConfig, "application/vnd.docker.container.image.v1+json":
		var ocispecImage ocispec.Image
		b, err := content.ReadBlob(ctx, containerdImage.ContentStore(), desc)
		if err != nil {
			return nil, fmt.Errorf("failed to read image config: %w", err)
		}
		if err := json.Unmarshal(b, &ocispecImage); err != nil {
			return nil, fmt.Errorf("failed to unmarshal image config: %w", err)
		}
		imgConfig = ocispecImage.Config
	default:
		return nil, fmt.Errorf("unknown media type %q", desc.MediaType)
	}

	snapshotter := options.GOptions.Snapshotter
	
	return &imgutil.EnsuredImage{
		Ref:         ref,
		Image:       containerdImage,
		ImageConfig: imgConfig,
		Snapshotter: snapshotter,
		Remote:      false, // IPFS is not a remote snapshotter
	}, nil
}

// PushImageWithTransfer pushes an image to IPFS using the Transfer Service.
func PushImageWithTransfer(ctx context.Context, client *containerd.Client, rawRef, ipfsPath string, options types.ImagePushOptions) (string, error) {
	// Build platform options
	platformsSlice, err := platformutil.NewOCISpecPlatformSlice(options.AllPlatforms, options.Platforms)
	if err != nil {
		return "", fmt.Errorf("failed to parse platforms: %w", err)
	}

	// Get the image from the image store
	imageService := client.ImageService()
	img, err := imageService.Get(ctx, rawRef)
	if err != nil {
		return "", fmt.Errorf("failed to get image %q: %w", rawRef, err)
	}

	// Create image store source
	storeOpts := []transferimage.StoreOpt{}
	if len(platformsSlice) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(platformsSlice...))
	}
	source := transferimage.NewStore(rawRef, storeOpts...)

	// Create IPFS destination
	destOpts := []IPFSDestinationOpt{
		WithContentStore(client.ContentStore()),
	}
	dest, err := NewIPFSDestination(ctx, ipfsPath, destOpts...)
	if err != nil {
		return "", fmt.Errorf("failed to create IPFS destination: %w", err)
	}

	// Prepare transfer options
	transferOpts := []transfer.Opt{}
	if !options.Quiet && options.Stdout != nil {
		pf, done := transferutil.ProgressHandler(ctx, options.Stdout)
		defer done()
		transferOpts = append(transferOpts, transfer.WithProgress(pf))
	}

	// Execute transfer
	if err := client.Transfer(ctx, source, dest, transferOpts...); err != nil {
		return "", fmt.Errorf("failed to transfer image to IPFS: %w", err)
	}

	// Push the root descriptor to IPFS
	// The root descriptor contains the manifest CID
	finalCID, err := dest.pushDescriptorToIPFS(ctx, img.Target)
	if err != nil {
		return "", fmt.Errorf("failed to push root descriptor: %w", err)
	}

	return finalCID, nil
}

// buildTransferStoreOptions builds store options for the transfer service.
func buildTransferStoreOptions(ctx context.Context, ref string, options types.ImagePullOptions) ([]transferimage.StoreOpt, bool, error) {
	var storeOpts []transferimage.StoreOpt

	// Add platform filters
	if len(options.OCISpecPlatform) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(options.OCISpecPlatform...))
	}

	// Determine if we should unpack
	unpackEnabled := len(options.OCISpecPlatform) == 1
	if options.Unpack != nil {
		unpackEnabled = *options.Unpack
		if unpackEnabled && len(options.OCISpecPlatform) != 1 {
			return nil, false, fmt.Errorf("unpacking requires a single platform to be specified (e.g., --platform=amd64)")
		}
	}

	// Add unpack configuration if enabled
	if unpackEnabled {
		platform := options.OCISpecPlatform[0]
		snapshotter := options.GOptions.Snapshotter
		storeOpts = append(storeOpts, transferimage.WithUnpack(platform, snapshotter))
	}

	return storeOpts, unpackEnabled, nil
}
