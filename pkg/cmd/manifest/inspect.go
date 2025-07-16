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

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/containerdutil"
	"github.com/containerd/nerdctl/v2/pkg/inspecttypes/native"
	"github.com/containerd/nerdctl/v2/pkg/manifestinspector"
	"github.com/containerd/nerdctl/v2/pkg/referenceutil"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func Inspect(ctx context.Context, client *containerd.Client, rawRef string, options types.ManifestInspectOptions) ([]any, error) {
	manifest, err := getManifest(ctx, client, rawRef, options)
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func inspectIdentifier(ctx context.Context, client *containerd.Client, identifier string) ([]images.Image, string, string, error) {
	parsedReference, err := referenceutil.Parse(identifier)
	if err != nil {
		return nil, "", "", err
	}
	digest := ""
	if parsedReference.Digest != "" {
		digest = parsedReference.Digest.String()
	}
	name := parsedReference.Name()
	tag := parsedReference.Tag

	var filters []string
	var imageList []images.Image

	if digest == "" {
		filters = []string{fmt.Sprintf("name==%s:%s", name, tag)}
		imageList, err = client.ImageService().List(ctx, filters...)
		if err != nil {
			return nil, "", "", fmt.Errorf("containerd image service failed: %w", err)
		}
		if len(imageList) == 0 {
			digest = fmt.Sprintf("sha256:%s.*", regexp.QuoteMeta(strings.TrimPrefix(identifier, "sha256:")))
			name = ""
			tag = ""
		} else {
			digest = imageList[0].Target.Digest.String()
		}
	}
	filters = []string{fmt.Sprintf("target.digest~=^%s$", digest)}
	imageList, err = client.ImageService().List(ctx, filters...)
	if err != nil {
		return nil, "", "", fmt.Errorf("containerd image service failed: %w", err)
	}

	if len(imageList) == 0 && digest != "" {
		imageList, err = findImageByManifestDigest(ctx, client, digest)
		if err != nil {
			return nil, "", "", fmt.Errorf("find image by manifest digest failed: %w", err)
		}
	}

	return imageList, name, tag, nil
}

func getManifest(ctx context.Context, client *containerd.Client, rawRef string, options types.ManifestInspectOptions) ([]any, error) {
	var errs []error
	var entries []interface{}

	candidateImageList, _, _, err := inspectIdentifier(ctx, client, rawRef)
	if err != nil {
		errs = append(errs, fmt.Errorf("%w: %s", err, rawRef))
		return nil, err
	}
	for _, candidateImage := range candidateImageList {
		entry, err := manifestinspector.Inspect(ctx, client, candidateImage)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s", err, candidateImage.Name))
			continue
		}
		// Process entry based on rawRef and verbose options
		processedEntry := processManifestEntry(entry, rawRef, options.Verbose)
		entries = append(entries, processedEntry)
	}
	if len(errs) > 0 {
		return []any{}, fmt.Errorf("%d errors:\n%w", len(errs), errors.Join(errs...))
	}

	if len(entries) == 0 {
		return []any{}, fmt.Errorf("no manifest found for %s", rawRef)
	}

	return entries, nil
}

// processManifestEntry processes manifest entry based on rawRef and verbose options
func processManifestEntry(entry *native.Manifest, rawRef string, verbose bool) *native.Manifest {
	parsedReference, err := referenceutil.Parse(rawRef)
	if err != nil {
		return entry
	}

	digest := ""
	if parsedReference.Digest != "" {
		digest = parsedReference.Digest.String()
	}
	result := &native.Manifest{}
	// If rawRef has no digest
	if digest == "" {
		if verbose {
			// verbose is true, output complete entry
			return entry
		}
		// verbose is false, decide output based on whether index is empty
		if entry.Index != nil {
			// index is not empty, only output Index
			result.Index = entry.Index
		} else {
			// index is empty, only output Manifest field from Manifests
			if len(entry.Manifests) == 1 {
				// If there's only one manifest, output it directly without the Manifests wrapper
				result.Manifests = []native.ManifestEntry{
					{
						Manifest: entry.Manifests[0].Manifest,
					},
				}
			} else {
				for _, manifestEntry := range entry.Manifests {
					result.Manifests = append(result.Manifests, native.ManifestEntry{
						Manifest: manifestEntry.Manifest,
					})
				}
			}
		}
		return result
	} else {
		// If rawRef has digest, find matching content

		// Check if digest matches index digest
		if entry.IndexDesc != nil && entry.IndexDesc.Digest.String() == digest {
			if verbose {
				// verbose is true, output complete entry but clear manifests field
				result.Index = entry.Index
				result.IndexDesc = entry.IndexDesc
			} else {
				// verbose is false, only output Index if index is not empty
				result.Index = entry.Index
			}
		} else {
			// Check if digest matches manifest digest
			for _, manifestEntry := range entry.Manifests {
				if manifestEntry.ManifestDesc != nil && manifestEntry.ManifestDesc.Digest.String() == digest {
					// Only keep matching manifestEntry
					if verbose {
						result.Manifests = append(result.Manifests, manifestEntry)
					} else {
						result.Manifests = append(result.Manifests, native.ManifestEntry{
							Manifest: manifestEntry.Manifest,
						})
					}
					break
				}
			}
		}
		return result
	}

}

func findImageByManifestDigest(
	ctx context.Context,
	client *containerd.Client,
	targetDigest string,
) ([]images.Image, error) {
	var resultList []images.Image
	imageList, err := client.ImageService().List(ctx)
	if err != nil {
		return nil, err
	}
	provider := containerdutil.NewProvider(client)
	for _, img := range imageList {
		desc := img.Target
		if images.IsIndexType(desc.MediaType) {
			indexData, err := containerdutil.ReadBlob(ctx, provider, desc)
			if err != nil {
				continue
			}
			var index ocispec.Index
			if err := json.Unmarshal(indexData, &index); err != nil {
				continue
			}
			for _, mani := range index.Manifests {
				if mani.Digest.String() == targetDigest {
					resultList = append(resultList, img)
				}
			}
		}
		if images.IsManifestType(desc.MediaType) && desc.Digest.String() == targetDigest {
			resultList = append(resultList, img)
		}
	}
	return resultList, nil
}
