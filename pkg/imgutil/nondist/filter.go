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

// Package nondist provides utilities for handling non-distributable blobs in container images.
package nondist

import (
	"context"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// IsNonDistributableMediaType checks if a media type represents a non-distributable blob.
// Non-distributable blobs are typically used for Windows base layers that cannot be
// freely distributed due to licensing restrictions.
func IsNonDistributableMediaType(mediaType string) bool {
	return strings.Contains(mediaType, "nondistributable") ||
		strings.Contains(mediaType, "foreign")
}

// FilteredStore wraps a content store to filter out non-distributable blobs.
// When allowNonDist is false, attempts to write non-distributable blobs will
// be intercepted and replaced with a no-op writer.
type FilteredStore struct {
	content.Store
	allowNonDist bool
}

// NewFilteredStore creates a new filtered content store.
func NewFilteredStore(store content.Store, allowNonDist bool) *FilteredStore {
	return &FilteredStore{
		Store:        store,
		allowNonDist: allowNonDist,
	}
}

// Writer intercepts write operations and returns a no-op writer for non-distributable blobs
// when they are not allowed.
func (fs *FilteredStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	var wOpts content.WriterOpts
	for _, opt := range opts {
		if err := opt(&wOpts); err != nil {
			return nil, err
		}
	}

	// If non-distributable blobs are not allowed and this is a non-distributable blob,
	// return a no-op writer that discards the content
	if !fs.allowNonDist && IsNonDistributableMediaType(wOpts.Desc.MediaType) {
		return &noopWriter{
			desc:      wOpts.Desc,
			startedAt: time.Now(),
		}, nil
	}

	return fs.Store.Writer(ctx, opts...)
}

// noopWriter is a writer that discards all content without actually writing to storage.
// This is used to skip uploading non-distributable blobs while maintaining the
// appearance of a successful write operation.
type noopWriter struct {
	desc      ocispec.Descriptor
	offset    int64
	startedAt time.Time
	updatedAt time.Time
}

func (w *noopWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	w.offset += int64(n)
	w.updatedAt = time.Now()
	return n, nil
}

func (w *noopWriter) Close() error {
	return nil
}

func (w *noopWriter) Digest() digest.Digest {
	return w.desc.Digest
}

func (w *noopWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	// Don't actually commit anything - just pretend it succeeded
	return nil
}

func (w *noopWriter) Status() (content.Status, error) {
	return content.Status{
		Ref:       "noop-" + w.desc.Digest.String(),
		Offset:    w.offset,
		Total:     w.desc.Size,
		StartedAt: w.startedAt,
		UpdatedAt: w.updatedAt,
	}, nil
}

func (w *noopWriter) Truncate(size int64) error {
	w.offset = size
	return nil
}
