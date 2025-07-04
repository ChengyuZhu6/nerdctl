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
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
)

// StreamConfig manages IO streams for a container and supports multiple client sessions.
// It implements Docker's multi-session IO sharing mechanism with broadcasters and input aggregation.
type StreamConfig struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed atomic.Bool

	// Broadcasters for output streams
	stdout *Broadcaster
	stderr *Broadcaster

	// Input aggregation
	stdinPipe   io.WriteCloser // Write end for sending input to container
	stdinReader io.ReadCloser  // Read end for container to read input

	// Direct IO connection to containerd
	dio *cio.DirectIO

	// Container metadata
	containerID string
	namespace   string
	tty         bool
}

// NewStreamConfig creates a new stream configuration for a container
func NewStreamConfig(containerID, namespace string, tty bool) *StreamConfig {
	// Create stdin pipe for input aggregation
	stdinReader, stdinPipe := io.Pipe()

	sc := &StreamConfig{
		stdout:      NewBroadcaster(),
		stderr:      NewBroadcaster(),
		stdinPipe:   stdinPipe,
		stdinReader: stdinReader,
		containerID: containerID,
		namespace:   namespace,
		tty:         tty,
	}

	return sc
}

// AttachClient creates a new BytesPipe set for a client session
func (sc *StreamConfig) AttachClient(stdin io.Reader, stdout, stderr io.Writer) (*ClientStreams, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.closed.Load() {
		return nil, io.ErrClosedPipe
	}

	// Create output pipes for this client
	var stdoutPipe, stderrPipe *BytesPipe

	if stdout != nil {
		stdoutPipe = NewBytesPipe()
		sc.stdout.AddWriter(stdoutPipe)

		// Start copying from pipe to client's stdout
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			defer stdoutPipe.Close()
			io.Copy(stdout, stdoutPipe)
		}()
	}

	if stderr != nil && !sc.tty {
		stderrPipe = NewBytesPipe()
		sc.stderr.AddWriter(stderrPipe)

		// Start copying from pipe to client's stderr
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			defer stderrPipe.Close()
			io.Copy(stderr, stderrPipe)
		}()
	}

	// Handle stdin input aggregation
	if stdin != nil {
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			io.Copy(sc.stdinPipe, stdin)
		}()
	}

	streams := &ClientStreams{
		stdout: stdoutPipe,
		stderr: stderrPipe,
		config: sc,
	}

	return streams, nil
}

// CopyToPipe establishes the bridge between containerd DirectIO and StreamConfig
func (sc *StreamConfig) CopyToPipe(dio *cio.DirectIO) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.closed.Load() || dio == nil {
		return
	}

	sc.dio = dio

	// Copy stdout from containerd to broadcaster
	if dio.Stdout != nil {
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			defer dio.Stdout.Close()

			buf := make([]byte, 32*1024) // 32KB buffer
			for {
				n, err := dio.Stdout.Read(buf)
				if n > 0 {
					sc.stdout.Write(buf[:n])
				}
				if err != nil {
					if err != io.EOF {
						log.G(context.Background()).WithError(err).Debug("stdout copy error")
					}
					break
				}
			}
		}()
	}

	// Copy stderr from containerd to broadcaster
	if dio.Stderr != nil && !sc.tty {
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			defer dio.Stderr.Close()

			buf := make([]byte, 32*1024) // 32KB buffer
			for {
				n, err := dio.Stderr.Read(buf)
				if n > 0 {
					sc.stderr.Write(buf[:n])
				}
				if err != nil {
					if err != io.EOF {
						log.G(context.Background()).WithError(err).Debug("stderr copy error")
					}
					break
				}
			}
		}()
	}

	// Copy aggregated stdin to containerd
	if dio.Stdin != nil {
		sc.wg.Add(1)
		go func() {
			defer sc.wg.Done()
			defer dio.Stdin.Close()
			io.Copy(dio.Stdin, sc.stdinReader)
		}()
	}
}

// StdinPipe returns the stdin pipe for input aggregation
func (sc *StreamConfig) StdinPipe() io.WriteCloser {
	return sc.stdinPipe
}

// Close closes the stream configuration and all associated resources
func (sc *StreamConfig) Close() error {
	if !sc.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Close stdin pipe to signal no more input
	if sc.stdinPipe != nil {
		sc.stdinPipe.Close()
	}
	if sc.stdinReader != nil {
		sc.stdinReader.Close()
	}

	// Close broadcasters
	sc.stdout.Close()
	sc.stderr.Close()

	// Wait for all goroutines to finish
	sc.wg.Wait()

	return nil
}

// Wait waits for all stream operations to complete
func (sc *StreamConfig) Wait() {
	sc.wg.Wait()
}

// IsClosed returns whether the stream config is closed
func (sc *StreamConfig) IsClosed() bool {
	return sc.closed.Load()
}

// ClientStreams represents the stream set for a client session
type ClientStreams struct {
	stdout *BytesPipe
	stderr *BytesPipe
	config *StreamConfig
}

// Close cleans up client streams
func (cs *ClientStreams) Close() error {
	if cs.stdout != nil {
		cs.config.stdout.RemoveWriter(cs.stdout)
		cs.stdout.Close()
	}
	if cs.stderr != nil {
		cs.config.stderr.RemoveWriter(cs.stderr)
		cs.stderr.Close()
	}
	return nil
}
