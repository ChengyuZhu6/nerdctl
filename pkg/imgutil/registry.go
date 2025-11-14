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
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/dockerconfigresolver"
	"github.com/containerd/nerdctl/v2/pkg/referenceutil"
)

// CustomOCIRegistry wraps registry.OCIRegistry with custom TLS configuration
type CustomOCIRegistry struct {
	reference string
	resolver  remotes.Resolver
}

// NewCustomOCIRegistry creates an OCIRegistry with proper TLS configuration for insecure registries
func NewCustomOCIRegistry(ctx context.Context, parsedReference *referenceutil.ImageReference, gOptions types.GlobalCommandOptions, plainHTTP bool) (*CustomOCIRegistry, error) {
	ch, err := dockerconfigresolver.NewCredentialHelper(parsedReference.Domain)
	if err != nil {
		return nil, err
	}

	// Prepare host options with TLS configuration
	hostOptions := dockerconfig.HostOptions{}

	// Set up host directory
	if len(gOptions.HostsDir) > 0 {
		hostOptions.HostDir = dockerconfig.HostDirFromRoot(gOptions.HostsDir[0])
	}

	// Set up credentials
	if ch != nil {
		hostOptions.Credentials = func(host string) (string, string, error) {
			creds, err := ch.GetCredentials(ctx, parsedReference.String(), host)
			if err != nil {
				return "", "", err
			}
			return creds.Username, creds.Secret, nil
		}
	}

	// Determine if this is a localhost registry
	isLocalHost, err := docker.MatchLocalhost(parsedReference.Domain)
	if err != nil {
		return nil, err
	}

	// Configure TLS and scheme based on insecure-registry flag
	if gOptions.InsecureRegistry {
		if plainHTTP || isLocalHost {
			// Plain HTTP: use http scheme and no TLS
			log.G(ctx).Warnf("using plain HTTP for registry %q", parsedReference.Domain)
			hostOptions.DefaultScheme = "http"
			// Set DefaultTLS to nil to avoid HTTPS attempts
			hostOptions.DefaultTLS = nil
		} else {
			// HTTPS but skip certificate verification
			log.G(ctx).Warnf("skipping verifying HTTPS certs for %q", parsedReference.Domain)
			hostOptions.DefaultTLS = &tls.Config{
				InsecureSkipVerify: true,
			}
		}
	} else if isLocalHost || plainHTTP {
		// Localhost without insecure flag: use http
		hostOptions.DefaultScheme = "http"
	}

	// Add UpdateClient to handle additional HTTP client configuration
	hostOptions.UpdateClient = func(client *http.Client) error {
		// Additional client configuration can be added here if needed
		return nil
	}

	// Create resolver with configured host options
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: dockerconfig.ConfigureHosts(ctx, hostOptions),
	})

	return &CustomOCIRegistry{
		reference: parsedReference.String(),
		resolver:  resolver,
	}, nil
}

// String returns a string representation of the registry
func (r *CustomOCIRegistry) String() string {
	return fmt.Sprintf("OCI Registry (%s)", r.reference)
}

// Image returns the image reference
func (r *CustomOCIRegistry) Image() string {
	return r.reference
}

// Resolve resolves the image reference to a descriptor
func (r *CustomOCIRegistry) Resolve(ctx context.Context) (name string, desc ocispec.Descriptor, err error) {
	return r.resolver.Resolve(ctx, r.reference)
}

// SetResolverOptions sets resolver options
func (r *CustomOCIRegistry) SetResolverOptions(options ...transfer.ImageResolverOption) {
	if resolver, ok := r.resolver.(remotes.ResolverWithOptions); ok {
		resolver.SetOptions(options...)
	}
}

// Fetcher returns a fetcher for the given reference
func (r *CustomOCIRegistry) Fetcher(ctx context.Context, ref string) (transfer.Fetcher, error) {
	return r.resolver.Fetcher(ctx, ref)
}

// Pusher returns a pusher for the given descriptor
func (r *CustomOCIRegistry) Pusher(ctx context.Context, desc ocispec.Descriptor) (transfer.Pusher, error) {
	ref := r.reference
	// Annotate ref with digest to push only push tag for single digest
	if len(ref) > 0 && ref[len(ref)-1] != '@' {
		ref = ref + "@" + desc.Digest.String()
	}
	return r.resolver.Pusher(ctx, ref)
}

// Ensure CustomOCIRegistry implements the required interfaces
var (
	_ transfer.ImageResolver = (*CustomOCIRegistry)(nil)
	_ transfer.ImageFetcher  = (*CustomOCIRegistry)(nil)
	_ transfer.ImagePusher   = (*CustomOCIRegistry)(nil)
)

// createOCIRegistryWithCustomTLS creates an OCIRegistry using our custom implementation
// that properly handles TLS configuration for insecure registries
func createOCIRegistryWithCustomTLS(ctx context.Context, parsedReference *referenceutil.ImageReference, gOptions types.GlobalCommandOptions, plainHTTP bool) (transfer.ImageFetcher, error) {
	return NewCustomOCIRegistry(ctx, parsedReference, gOptions, plainHTTP)
}
