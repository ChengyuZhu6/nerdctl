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
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/stargz-snapshotter/ipfs"
)

// Registry implements transfer.ImageFetcher interface for IPFS protocol.
// It wraps the existing IPFS resolver to work with containerd's transfer service.
type Registry struct {
	reference string
	scheme    string
	ipfsPath  string
	resolver  remotes.Resolver
}

// NewRegistry creates a new IPFS registry for use with transfer API.
// scheme should be "ipfs" or "ipns".
// ipfsPath is the path to IPFS repository (optional, defaults to ~/.ipfs or $IPFS_PATH).
func NewRegistry(ctx context.Context, ref, scheme, ipfsPath string) (*Registry, error) {
	if scheme != "ipfs" && scheme != "ipns" {
		return nil, fmt.Errorf("unsupported IPFS scheme: %q (must be 'ipfs' or 'ipns')", scheme)
	}

	resolver, err := ipfs.NewResolver(ipfs.ResolverOptions{
		Scheme:   scheme,
		IPFSPath: ipfsPath,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create IPFS resolver: %w", err)
	}

	return &Registry{
		reference: ref,
		scheme:    scheme,
		ipfsPath:  ipfsPath,
		resolver:  resolver,
	}, nil
}

// Resolve implements transfer.ImageResolver interface.
// It resolves the IPFS reference (CID) to an OCI descriptor.
func (r *Registry) Resolve(ctx context.Context) (name string, desc ocispec.Descriptor, err error) {
	return r.resolver.Resolve(ctx, r.reference)
}

// Fetcher implements transfer.ImageFetcher interface.
// It returns a fetcher that can retrieve content from IPFS.
func (r *Registry) Fetcher(ctx context.Context, ref string) (transfer.Fetcher, error) {
	return r.resolver.Fetcher(ctx, ref)
}

// String implements fmt.Stringer interface for better logging.
func (r *Registry) String() string {
	return fmt.Sprintf("IPFS Registry (%s://%s)", r.scheme, r.reference)
}
