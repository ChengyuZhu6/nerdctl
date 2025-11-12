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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/log"
	ipfsclient "github.com/containerd/stargz-snapshotter/ipfs/client"
)

// IPFSDestination implements transfer.ImagePusher for pushing images to IPFS.
type IPFSDestination struct {
	ipfsPath      string
	ipfsclient    *ipfsclient.Client
	ipfsAPIURL    string // IPFS HTTP API URL
	layerConvert  converter.ConvertFunc
	contentStore  content.Store
	pushedCIDs    map[string]string // digest -> CID mapping
	httpClient    *http.Client
}

// IPFSDestinationOpt is an option for configuring IPFSDestination.
type IPFSDestinationOpt func(*IPFSDestination)

// WithLayerConverter sets the layer converter function.
func WithLayerConverter(convert converter.ConvertFunc) IPFSDestinationOpt {
	return func(d *IPFSDestination) {
		d.layerConvert = convert
	}
}

// WithContentStore sets the content store for reading blobs.
func WithContentStore(cs content.Store) IPFSDestinationOpt {
	return func(d *IPFSDestination) {
		d.contentStore = cs
	}
}

// NewIPFSDestination creates a new IPFS destination for pushing images.
func NewIPFSDestination(ctx context.Context, ipfsPath string, opts ...IPFSDestinationOpt) (*IPFSDestination, error) {
	ipath := lookupIPFSPath(ipfsPath)
	iurl, err := ipfsclient.GetIPFSAPIAddress(ipath, "http")
	if err != nil {
		return nil, fmt.Errorf("failed to get IPFS API address: %w", err)
	}

	dest := &IPFSDestination{
		ipfsPath:   ipath,
		ipfsclient: ipfsclient.New(iurl),
		ipfsAPIURL: iurl,
		pushedCIDs: make(map[string]string),
		httpClient: &http.Client{},
	}

	for _, opt := range opts {
		opt(dest)
	}

	return dest, nil
}

// String returns a string representation of the IPFS destination.
func (d *IPFSDestination) String() string {
	return "IPFS Destination"
}

// Pusher returns a pusher that can push blobs to IPFS.
func (d *IPFSDestination) Pusher(ctx context.Context, desc ocispec.Descriptor) (transfer.Pusher, error) {
	return &ipfsPusher{
		dest: d,
		ctx:  ctx,
	}, nil
}

// GetRootCID returns the root CID after all blobs have been pushed.
// This should be called after the transfer is complete.
func (d *IPFSDestination) GetRootCID(rootDesc ocispec.Descriptor) (string, error) {
	cid, ok := d.pushedCIDs[rootDesc.Digest.String()]
	if !ok {
		return "", fmt.Errorf("root descriptor not found in pushed CIDs")
	}
	return cid, nil
}

// ipfsPusher implements transfer.Pusher for pushing blobs to IPFS.
type ipfsPusher struct {
	dest *IPFSDestination
	ctx  context.Context
}

// Push pushes a blob to IPFS and returns a content writer.
func (p *ipfsPusher) Push(ctx context.Context, desc ocispec.Descriptor) (content.Writer, error) {
	// Check if we already pushed this blob
	if cid, ok := p.dest.pushedCIDs[desc.Digest.String()]; ok {
		log.G(ctx).Debugf("blob %s already pushed to IPFS as CID %s", desc.Digest, cid)
		return &nopWriter{}, nil
	}

	// If we have a content store, read the blob from it
	if p.dest.contentStore != nil {
		return p.pushFromContentStore(ctx, desc)
	}

	// Otherwise, return a writer that will push to IPFS when written to
	return &ipfsWriter{
		pusher: p,
		desc:   desc,
		buf:    make([]byte, 0, desc.Size),
	}, nil
}

// pushFromContentStore reads a blob from the content store and pushes it to IPFS.
func (p *ipfsPusher) pushFromContentStore(ctx context.Context, desc ocispec.Descriptor) (content.Writer, error) {
	// Read the blob from content store
	ra, err := p.dest.contentStore.ReaderAt(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("failed to get reader for blob %s: %w", desc.Digest, err)
	}
	defer ra.Close()

	// Read all content
	data := make([]byte, desc.Size)
	if _, err := ra.ReadAt(data, 0); err != nil {
		return nil, fmt.Errorf("failed to read blob %s: %w", desc.Digest, err)
	}

	// Push to IPFS
	cid, err := p.pushToIPFS(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("failed to push blob %s to IPFS: %w", desc.Digest, err)
	}

	// Store the CID mapping
	p.dest.pushedCIDs[desc.Digest.String()] = cid
	log.G(ctx).Debugf("pushed blob %s to IPFS as CID %s", desc.Digest, cid)

	// Update descriptor URLs to include IPFS CID
	desc.URLs = append(desc.URLs, "ipfs://"+cid)

	return &nopWriter{}, nil
}

// pushToIPFS pushes data to IPFS and returns the CID.
func (p *ipfsPusher) pushToIPFS(ctx context.Context, data []byte) (string, error) {
	return p.dest.addToIPFS(ctx, data)
}

// addToIPFS adds data to IPFS using the HTTP API and returns the CID.
func (d *IPFSDestination) addToIPFS(ctx context.Context, data []byte) (string, error) {
	// Create multipart form data
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	
	// Add the file part
	part, err := writer.CreateFormFile("file", "blob")
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("failed to write data to form: %w", err)
	}
	
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close multipart writer: %w", err)
	}
	
	// Build the API URL
	apiURL := d.ipfsAPIURL + "/api/v0/add"
	
	// Add query parameters
	params := url.Values{}
	params.Add("pin", "true")           // Pin the content
	params.Add("quiet", "true")         // Only return the hash
	params.Add("raw-leaves", "true")    // Use raw leaves for better deduplication
	params.Add("cid-version", "1")      // Use CIDv1
	apiURL += "?" + params.Encode()
	
	// Create the request
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	
	// Send the request
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to IPFS: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("IPFS API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	
	// Parse the response
	var result struct {
		Hash string `json:"Hash"`
		Name string `json:"Name"`
		Size string `json:"Size"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode IPFS response: %w", err)
	}
	
	if result.Hash == "" {
		return "", fmt.Errorf("IPFS API returned empty hash")
	}
	
	log.G(ctx).Debugf("Added blob to IPFS: CID=%s, Size=%s", result.Hash, result.Size)
	return result.Hash, nil
}

// ipfsWriter is a content.Writer that buffers data and pushes to IPFS on commit.
type ipfsWriter struct {
	pusher *ipfsPusher
	desc   ocispec.Descriptor
	buf    []byte
}

func (w *ipfsWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *ipfsWriter) Close() error {
	return nil
}

func (w *ipfsWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	// Push the buffered data to IPFS
	cid, err := w.pusher.pushToIPFS(ctx, w.buf)
	if err != nil {
		return fmt.Errorf("failed to push to IPFS: %w", err)
	}

	// Store the CID mapping
	w.pusher.dest.pushedCIDs[w.desc.Digest.String()] = cid
	log.G(ctx).Debugf("committed blob %s to IPFS as CID %s", w.desc.Digest, cid)

	// Update descriptor URLs
	w.desc.URLs = append(w.desc.URLs, "ipfs://"+cid)

	return nil
}

func (w *ipfsWriter) Digest() digest.Digest {
	return w.desc.Digest
}

func (w *ipfsWriter) Status() (content.Status, error) {
	return content.Status{
		Ref:    w.desc.Digest.String(),
		Offset: int64(len(w.buf)),
		Total:  w.desc.Size,
	}, nil
}

func (w *ipfsWriter) Truncate(size int64) error {
	if size < int64(len(w.buf)) {
		w.buf = w.buf[:size]
	}
	return nil
}

// nopWriter is a no-op writer for blobs that have already been pushed.
type nopWriter struct{}

func (w *nopWriter) Write(p []byte) (int, error)                                                { return len(p), nil }
func (w *nopWriter) Close() error                                                               { return nil }
func (w *nopWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	return nil
}
func (w *nopWriter) Digest() digest.Digest           { return "" }
func (w *nopWriter) Status() (content.Status, error) { return content.Status{}, nil }
func (w *nopWriter) Truncate(size int64) error       { return nil }

// pushDescriptorToIPFS pushes a descriptor JSON to IPFS and returns its CID.
func (d *IPFSDestination) pushDescriptorToIPFS(ctx context.Context, desc ocispec.Descriptor) (string, error) {
	// Marshal descriptor to JSON
	data, err := json.Marshal(desc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal descriptor: %w", err)
	}

	// Push to IPFS
	cid, err := d.addToIPFS(ctx, data)
	if err != nil {
		return "", fmt.Errorf("failed to push descriptor to IPFS: %w", err)
	}

	log.G(ctx).Debugf("Pushed descriptor to IPFS: CID=%s, Digest=%s", cid, desc.Digest)
	return cid, nil
}
