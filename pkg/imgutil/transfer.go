package imgutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/transfer"
	transferimage "github.com/containerd/containerd/v2/core/transfer/image"
	"github.com/containerd/containerd/v2/core/transfer/registry"
	"github.com/containerd/log"
	"github.com/containerd/platforms"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/errutil"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/dockerconfigresolver"
	"github.com/containerd/nerdctl/v2/pkg/platformutil"
	"github.com/containerd/nerdctl/v2/pkg/referenceutil"
	"github.com/containerd/nerdctl/v2/pkg/transferutil"
)

func ensureImageWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, rawRef string, options types.ImagePullOptions) (*EnsuredImage, error) {
	registryOpts, err := NewRegistryOpts(ctx, parsedReference.Domain, options.GOptions)
	if err != nil {
		return nil, err
	}
	return PullImageWithTransfer(ctx, client, parsedReference, rawRef, options, registryOpts)
}

func PullImageWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, rawRef string, options types.ImagePullOptions, registryOpts []registry.Opt) (*EnsuredImage, error) {
	storeOpts, unpackEnabled, err := buildTransferStoreOptions(ctx, parsedReference, options)
	if err != nil {
		return nil, err
	}
	store := transferimage.NewStore(parsedReference.String(), storeOpts...)

	progressWriter := options.Stderr
	if options.ProgressOutputToStdout {
		progressWriter = options.Stdout
	}

	transferErr := executeTransferWithRetry(ctx, client, parsedReference, options.GOptions, options.Quiet, progressWriter, func(plainHTTP bool) (interface{}, interface{}, error) {
		opts := append([]registry.Opt{}, registryOpts...)
		if plainHTTP {
			opts = append(opts, registry.WithDefaultScheme("http"))
		}
		fetcher, err := registry.NewOCIRegistry(ctx, parsedReference.String(), opts...)
		return fetcher, store, err
	})
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
	if unpackEnabled {
		log.G(ctx).Debugf("The image has been unpacked for snapshotter %q.", snapshotter)
	} else {
		log.G(ctx).Debugf("The image was not unpacked. Platforms=%v.", options.OCISpecPlatform)
	}

	return &EnsuredImage{
		Ref:         rawRef,
		Image:       containerdImage,
		ImageConfig: *imgConfig,
		Snapshotter: snapshotter,
		Remote:      snOpt.isRemote(),
	}, nil
}

func PushWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, pushRef, rawRef string, options types.ImagePushOptions) error {
	registryOpts, err := NewRegistryOpts(ctx, parsedReference.Domain, options.GOptions)
	if err != nil {
		return err
	}
	return PushImageWithTransfer(ctx, client, parsedReference, pushRef, rawRef, options, registryOpts)
}

func PushImageWithTransfer(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, pushRef, rawRef string, options types.ImagePushOptions, registryOpts []registry.Opt) error {
	platformsSlice, err := platformutil.NewOCISpecPlatformSlice(options.AllPlatforms, options.Platforms)
	if err != nil {
		return err
	}

	storeOpts := []transferimage.StoreOpt{}
	if len(platformsSlice) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(platformsSlice...))
	}

	source := transferimage.NewStore(pushRef, storeOpts...)
	progressWriter := io.Discard
	if options.Stdout != nil {
		progressWriter = options.Stdout
	}

	transferErr := executeTransferWithRetry(ctx, client, parsedReference, options.GOptions, options.Quiet, progressWriter, func(plainHTTP bool) (interface{}, interface{}, error) {
		opts := append([]registry.Opt{}, registryOpts...)
		if plainHTTP {
			opts = append(opts, registry.WithDefaultScheme("http"))
		}
		pusher, err := registry.NewOCIRegistry(ctx, parsedReference.String(), opts...)
		return source, pusher, err
	})
	if transferErr != nil {
		return transferErr
	}

	log.G(ctx).Debugf("pushed %q via transfer service", rawRef)
	return nil
}

func buildTransferStoreOptions(ctx context.Context, parsedReference *referenceutil.ImageReference, options types.ImagePullOptions) ([]transferimage.StoreOpt, bool, error) {
	var storeOpts []transferimage.StoreOpt
	if len(options.OCISpecPlatform) > 0 {
		storeOpts = append(storeOpts, transferimage.WithPlatforms(options.OCISpecPlatform...))
	}

	unpackEnabled := len(options.OCISpecPlatform) == 1
	if options.Unpack != nil {
		unpackEnabled = *options.Unpack
		if unpackEnabled && len(options.OCISpecPlatform) != 1 {
			return nil, false, fmt.Errorf("unpacking requires a single platform to be specified (e.g., --platform=amd64)")
		}
	}

	if unpackEnabled {
		platform := options.OCISpecPlatform[0]
		snapshotter := options.GOptions.Snapshotter
		storeOpts = append(storeOpts, transferimage.WithUnpack(platform, snapshotter))
		log.G(ctx).Debugf("The image will be unpacked for platform %q, snapshotter %q.", platforms.Format(platforms.Normalize(platform)), snapshotter)
	}

	return storeOpts, unpackEnabled, nil
}

func executeTransferWithRetry(ctx context.Context, client *containerd.Client, parsedReference *referenceutil.ImageReference, gOptions types.GlobalCommandOptions, quiet bool, progressWriter io.Writer, createTransferables func(plainHTTP bool) (interface{}, interface{}, error)) error {
	doTransfer := func(plainHTTP bool) error {
		src, dst, err := createTransferables(plainHTTP)
		if err != nil {
			return err
		}

		opts := make([]transfer.Opt, 0, 1)
		var finish func()
		if quiet {
			finish = func() {}
		} else {
			pf, done := transferutil.ProgressHandler(ctx, progressWriter)
			finish = done
			opts = append(opts, transfer.WithProgress(pf))
		}
		err = client.Transfer(ctx, src, dst, opts...)
		finish()
		return err
	}

	transferErr := doTransfer(false)
	if transferErr != nil {
		if errors.Is(transferErr, http.ErrSchemeMismatch) || errutil.IsErrConnectionRefused(transferErr) {
			if gOptions.InsecureRegistry {
				log.G(ctx).WithError(transferErr).Warnf("server %q does not seem to support HTTPS, falling back to plain HTTP", parsedReference.Domain)
				transferErr = doTransfer(true)
			} else {
				log.G(ctx).WithError(transferErr).Errorf("server %q does not seem to support HTTPS", parsedReference.Domain)
				log.G(ctx).Info("Hint: you may want to try --insecure-registry to allow plain HTTP (if you are in a trusted network)")
				return transferErr
			}
		} else {
			return transferErr
		}
	}
	return transferErr
}

func NewRegistryOpts(ctx context.Context, domain string, gOptions types.GlobalCommandOptions) ([]registry.Opt, error) {
	ch, err := dockerconfigresolver.NewCredentialHelper(domain)
	if err != nil {
		return nil, err
	}

	opts := []registry.Opt{
		registry.WithCredentials(ch),
	}

	if len(gOptions.HostsDir) > 0 {
		opts = append(opts, registry.WithHostDir(gOptions.HostsDir[0]))
	}

	return opts, nil
}
