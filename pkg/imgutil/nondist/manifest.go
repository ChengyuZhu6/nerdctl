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

package nondist

import (
	"context"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// WalkManifestAndFilter walks through the image manifest and filters non-distributable blobs.
// This is called before pushing to ensure non-distributable blobs are not uploaded.
func WalkManifestAndFilter(ctx context.Context, client *containerd.Client, ref string, allowNonDist bool) error {
	if allowNonDist {
		// No filtering needed
		return nil
	}

	img, err := client.ImageService().Get(ctx, ref)
	if err != nil {
		return err
	}

	// Walk the manifest tree and check for non-distributable blobs
	hasNonDist := false
	handler := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if IsNonDistributableMediaType(desc.MediaType) {
			hasNonDist = true
			log.G(ctx).WithField("digest", desc.Digest).
				WithField("mediaType", desc.MediaType).
				Debug("Found non-distributable blob, will skip during push")
		}
		return nil, nil
	})

	if err := images.Walk(ctx, handler, img.Target); err != nil {
		return err
	}

	if hasNonDist {
		log.G(ctx).Info("Image contains non-distributable blobs that will be skipped during push")
	}

	return nil
}
