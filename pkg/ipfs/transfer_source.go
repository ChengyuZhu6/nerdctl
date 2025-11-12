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
	"io"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/transfer"
	ipfsclient "github.com/containerd/stargz-snapshotter/ipfs/client"
)

// IPFSSource implements transfer.ImageFetcher for pulling images from IPFS.
type IPFSSource struct {
	scheme     string // "ipfs" or "ipns"
	ref        string // IPFS CID or IPNS name
	ipfsPath   string // IPFS daemon path
	ipfsclient *ipfsclient.Client
}

// NewIPFSSource creates a new IPFS source for pulling images.
func NewIPFSSource(ctx context.Context, scheme, ref, ipfsPath string) (*IPFSSource, error) {
	if scheme != "ipfs" && scheme != "ipns" {
		return nil, fmt.Errorf("unsupported IPFS scheme: %q (expected ipfs or ipns)", scheme)
	}

	ipath := lookupIPFSPath(ipfsPath)
	iurl, err := ipfsclient.GetIPFSAPIAddress(ipath, "http")
	if err != nil {
		return nil, fmt.Errorf("failed to get IPFS API address: %w", err)
	}

	return &IPFSSource{
		scheme:     scheme,
		ref:        ref,
		ipfsPath:   ipath,
		ipfsclient: ipfsclient.New(iurl),
	}, nil
}

// String returns a string representation of the IPFS source.
func (s *IPFSSource) String() string {
	return fmt.Sprintf("IPFS Source (%s://%s)", s.scheme, s.ref)
}

// Resolve resolves the IPFS reference to an image descriptor.
// It fetches the root descriptor from IPFS and returns the manifest descriptor.
func (s *IPFSSource) Resolve(ctx context.Context) (string, ocispec.Descriptor, error) {
	// Get the root descriptor from IPFS
	// The CID should point to a descriptor JSON that contains the manifest reference
	rootDesc, err := s.fetchDescriptor(ctx, s.ref)
	if err != nil {
		return "", ocispec.Descriptor{}, fmt.Errorf("failed to fetch root descriptor from IPFS: %w", err)
	}

	// The root descriptor should contain the IPFS CID in its URLs
	// Verify it has the expected structure
	if len(rootDesc.URLs) == 0 {
		return "", ocispec.Descriptor{}, fmt.Errorf("root descriptor has no URLs")
	}

	// Return the reference and descriptor
	// The ref is used as the image name
	return s.ref, rootDesc, nil
}

// Fetcher returns a fetcher that can fetch blobs from IPFS.
func (s *IPFSSource) Fetcher(ctx context.Context, ref string) (transfer.Fetcher, error) {
	return &ipfsFetcher{
		ipfsclient: s.ipfsclient,
	}, nil
}

// SetResolverOptions sets resolver options (no-op for IPFS).
func (s *IPFSSource) SetResolverOptions(opts ...transfer.ImageResolverOption) {
	// IPFS doesn't use resolver options like concurrent downloads
	// This is a no-op to satisfy the ImageResolverOptionSetter interface
}

// fetchDescriptor fetches and decodes a descriptor from IPFS by CID.
func (s *IPFSSource) fetchDescriptor(ctx context.Context, cid string) (ocispec.Descriptor, error) {
	// Fetch the content from IPFS
	off, size := 0, 0 // size=0 means read all
	rc, err := s.ipfsclient.Get("/ipfs/"+cid, &off, &size)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to get IPFS content for CID %s: %w", cid, err)
	}
	defer rc.Close()

	// Decode the descriptor
	var desc ocispec.Descriptor
	if err := json.NewDecoder(rc).Decode(&desc); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to decode descriptor from CID %s: %w", cid, err)
	}

	return desc, nil
}

// ipfsFetcher implements transfer.Fetcher for fetching blobs from IPFS.
type ipfsFetcher struct {
	ipfsclient *ipfsclient.Client
}

// Fetch fetches a blob from IPFS using the CID stored in the descriptor's URLs.
func (f *ipfsFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	// Extract the IPFS CID from the descriptor's URLs
	cid, err := getIPFSCIDFromDescriptor(desc)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPFS CID from descriptor: %w", err)
	}

	// Fetch the content from IPFS
	off, size := 0, int(desc.Size)
	rc, err := f.ipfsclient.Get("/ipfs/"+cid, &off, &size)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blob from IPFS (CID: %s): %w", cid, err)
	}

	return rc, nil
}

// getIPFSCIDFromDescriptor extracts the IPFS CID from a descriptor's URLs field.
func getIPFSCIDFromDescriptor(desc ocispec.Descriptor) (string, error) {
	for _, u := range desc.URLs {
		if strings.HasPrefix(u, "ipfs://") {
			// Extract CID from ipfs://<CID>
			return u[7:], nil
		}
	}
	return "", fmt.Errorf("no IPFS CID found in descriptor URLs (digest: %s)", desc.Digest)
}
