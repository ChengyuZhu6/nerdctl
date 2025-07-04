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

package streamutil

import (
	"io"
	"sync"

	"github.com/containerd/log"
)

// Broadcaster implements Docker's unbuffered multi-client output sharing mechanism.
// It broadcasts data to all connected clients and automatically removes failed clients.
type Broadcaster struct {
	mu      sync.Mutex
	writers []io.WriteCloser // Multiple client writers
	closed  bool
}

// NewBroadcaster creates a new broadcaster
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		writers: make([]io.WriteCloser, 0),
	}
}

// AddWriter adds a new writer to the broadcaster
func (b *Broadcaster) AddWriter(w io.WriteCloser) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	b.writers = append(b.writers, w)
}

// RemoveWriter removes a writer from the broadcaster
func (b *Broadcaster) RemoveWriter(w io.WriteCloser) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, writer := range b.writers {
		if writer == w {
			// Remove this writer
			b.writers = append(b.writers[:i], b.writers[i+1:]...)
			break
		}
	}
}

// Write broadcasts data to all connected clients
func (b *Broadcaster) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, io.ErrClosedPipe
	}

	var evict []int
	for i, w := range b.writers {
		if n, err := w.Write(p); err != nil || n != len(p) {
			// Mark this writer for removal due to failure
			evict = append(evict, i)
			if err != nil {
				log.L.Debugf("Writer failed, removing: %v", err)
			}
		}
	}

	// Remove failed writers (in reverse order to maintain indices)
	for i := len(evict) - 1; i >= 0; i-- {
		idx := evict[i]
		// Close the failed writer
		if err := b.writers[idx].Close(); err != nil {
			log.L.Debugf("Error closing failed writer: %v", err)
		}
		// Remove from slice
		b.writers = append(b.writers[:idx], b.writers[idx+1:]...)
	}

	return len(p), nil
}

// Close closes the broadcaster and all writers
func (b *Broadcaster) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	b.closed = true

	var lastErr error
	for _, w := range b.writers {
		if err := w.Close(); err != nil {
			lastErr = err
		}
	}

	b.writers = nil
	return lastErr
}

// ClientCount returns the number of connected clients
func (b *Broadcaster) ClientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.writers)
}

// IsClosed returns whether the broadcaster is closed
func (b *Broadcaster) IsClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}
