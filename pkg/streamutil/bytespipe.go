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
	"time"
)

const (
	// defaultBufferSize is the initial buffer size
	defaultBufferSize = 64 * 1024 // 64KB
	// maxBufferSize is the maximum buffer size to prevent memory exhaustion
	maxBufferSize = 1024 * 1024 // 1MB
	// bufferGrowthFactor determines how much the buffer grows when full
	bufferGrowthFactor = 2
)

// BytesPipe implements Docker's BytesPipe for independent client buffering.
// Each client gets its own pipe with dynamic buffer management and backpressure control.
type BytesPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	r, w   int // read and write positions
	closed bool
	err    error

	// Backpressure and flow control
	writeWaiter chan struct{} // signals when space is available for writing
	readWaiter  chan struct{} // signals when data is available for reading
}

// NewBytesPipe creates a new BytesPipe with dynamic buffering
func NewBytesPipe() *BytesPipe {
	bp := &BytesPipe{
		buf:         make([]byte, defaultBufferSize),
		writeWaiter: make(chan struct{}, 1),
		readWaiter:  make(chan struct{}, 1),
	}
	bp.cond = sync.NewCond(&bp.mu)
	return bp
}

// Read reads data from the pipe
func (bp *BytesPipe) Read(p []byte) (int, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	for {
		// Check if there's data available
		if bp.r != bp.w {
			// Calculate available data
			available := bp.w - bp.r
			n := copy(p, bp.buf[bp.r:bp.r+available])
			bp.r += n

			// If buffer is empty, reset positions to optimize space
			if bp.r == bp.w {
				bp.r, bp.w = 0, 0
			}

			// Signal that space is available for writing
			select {
			case bp.writeWaiter <- struct{}{}:
			default:
			}

			return n, nil
		}

		// Check if pipe is closed
		if bp.closed {
			return 0, bp.err
		}

		// Wait for data to become available
		bp.cond.Wait()
	}
}

// Write writes data to the pipe
func (bp *BytesPipe) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.closed {
		return 0, io.ErrClosedPipe
	}

	written := 0
	for written < len(p) {
		// Check available space in buffer
		available := len(bp.buf) - bp.w
		needed := len(p) - written

		// If we need more space, try to grow the buffer
		if available < needed {
			if err := bp.growBuffer(needed); err != nil {
				// If we can't grow, write what we can
				if available == 0 {
					// Buffer is completely full, wait for reader
					bp.mu.Unlock()
					select {
					case <-bp.writeWaiter:
					case <-time.After(100 * time.Millisecond): // Prevent deadlock
					}
					bp.mu.Lock()
					continue
				}
				needed = available
			} else {
				available = len(bp.buf) - bp.w
			}
		}

		// Write as much as we can
		toWrite := needed
		if toWrite > available {
			toWrite = available
		}

		copy(bp.buf[bp.w:], p[written:written+toWrite])
		bp.w += toWrite
		written += toWrite

		// Signal that data is available for reading
		bp.cond.Signal()
		select {
		case bp.readWaiter <- struct{}{}:
		default:
		}
	}

	return written, nil
}

// growBuffer attempts to grow the buffer to accommodate more data
func (bp *BytesPipe) growBuffer(needed int) error {
	currentSize := len(bp.buf)

	// Calculate new size
	newSize := currentSize
	for newSize-bp.w < needed && newSize < maxBufferSize {
		newSize *= bufferGrowthFactor
	}

	// Don't grow beyond maximum size
	if newSize > maxBufferSize {
		newSize = maxBufferSize
	}

	// If we can't accommodate the needed space, return error
	if newSize-bp.w < needed && newSize <= currentSize {
		return io.ErrShortBuffer
	}

	// Create new buffer and copy existing data
	newBuf := make([]byte, newSize)
	copy(newBuf, bp.buf[bp.r:bp.w])

	// Update positions
	bp.w = bp.w - bp.r
	bp.r = 0
	bp.buf = newBuf

	return nil
}

// Close closes the pipe
func (bp *BytesPipe) Close() error {
	return bp.CloseWithError(io.EOF)
}

// CloseWithError closes the pipe with an error
func (bp *BytesPipe) CloseWithError(err error) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.closed {
		return nil
	}

	bp.closed = true
	if err == nil {
		bp.err = io.EOF
	} else {
		bp.err = err
	}

	// Wake up any waiting readers
	bp.cond.Broadcast()

	// Close channels
	close(bp.writeWaiter)
	close(bp.readWaiter)

	return nil
}

// IsClosed returns whether the pipe is closed
func (bp *BytesPipe) IsClosed() bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.closed
}

// BufferSize returns the current buffer size
func (bp *BytesPipe) BufferSize() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.buf)
}

// DataAvailable returns the amount of data available for reading
func (bp *BytesPipe) DataAvailable() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.w - bp.r
}
