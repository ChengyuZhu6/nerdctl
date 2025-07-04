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

package cioutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"

	"github.com/containerd/nerdctl/v2/pkg/streamutil"
)

const binaryIOProcTermTimeout = 12 * time.Second // Give logger process 10 seconds for cleanup

// ncio is a basic container IO implementation.
type ncio struct {
	cmd     *exec.Cmd
	config  cio.Config
	wg      *sync.WaitGroup
	closers []io.Closer
	cancel  context.CancelFunc
}

var bufPool = sync.Pool{
	New: func() interface{} {
		buffer := make([]byte, 32<<10)
		return &buffer
	},
}

func (c *ncio) Config() cio.Config {
	return c.config
}

func (c *ncio) Wait() {
	if c.wg != nil {
		c.wg.Wait()
	}
}

func (c *ncio) Close() error {

	var lastErr error

	if c.cmd != nil && c.cmd.Process != nil {

		// Send SIGTERM first, so logger process has a chance to flush and exit properly
		if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			lastErr = fmt.Errorf("failed to send SIGTERM: %w", err)

			if err := c.cmd.Process.Kill(); err != nil {
				lastErr = errors.Join(lastErr, fmt.Errorf("failed to kill process after faulty SIGTERM: %w", err))
			}

		}

		done := make(chan error, 1)
		go func() {
			done <- c.cmd.Wait()
		}()

		select {
		case err := <-done:
			return err
		case <-time.After(binaryIOProcTermTimeout):

			err := c.cmd.Process.Kill()
			if err != nil {
				lastErr = fmt.Errorf("failed to kill shim logger process: %w", err)
			}

		}
	}

	for _, closer := range c.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (c *ncio) Cancel() {
	if c.cancel != nil {
		c.cancel()
	}
}

func NewContainerIO(namespace string, logURI string, tty bool, stdin io.Reader, stdout, stderr io.Writer) cio.Creator {
	return func(id string) (_ cio.IO, err error) {
		var (
			cmd     *exec.Cmd
			closers []func() error
			streams = &cio.Streams{
				Terminal: tty,
			}
		)

		defer func() {
			if err == nil {
				return
			}
			result := []error{err}
			for _, fn := range closers {
				result = append(result, fn())
			}
			err = errors.Join(result...)
		}()

		if stdin != nil {
			streams.Stdin = stdin
		}

		var stdoutWriters []io.Writer
		if stdout != nil {
			stdoutWriters = append(stdoutWriters, stdout)
		}

		var stderrWriters []io.Writer
		if stderr != nil {
			stderrWriters = append(stderrWriters, stderr)
		}

		if runtime.GOOS != "windows" && logURI != "" && logURI != "none" {
			// starting logging binary logic is from https://github.com/containerd/containerd/blob/194a1fdd2cde35bc019ef138f30485e27fe0913e/cmd/containerd-shim-runc-v2/process/io.go#L247
			stdoutr, stdoutw, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			closers = append(closers, stdoutr.Close, stdoutw.Close)

			stderrr, stderrw, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			closers = append(closers, stderrr.Close, stderrw.Close)

			r, w, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			closers = append(closers, r.Close, w.Close)

			u, err := url.Parse(logURI)
			if err != nil {
				return nil, err
			}
			cmd = process.NewBinaryCmd(u, id, namespace)
			cmd.ExtraFiles = append(cmd.ExtraFiles, stdoutr, stderrr, w)

			if err := cmd.Start(); err != nil {
				return nil, fmt.Errorf("failed to start binary process with cmdArgs %v (logURI: %s): %w", cmd.Args, logURI, err)
			}

			closers = append(closers, func() error { return cmd.Process.Kill() })

			// close our side of the pipe after start
			if err := w.Close(); err != nil {
				return nil, fmt.Errorf("failed to close write pipe after start: %w", err)
			}

			// wait for the logging binary to be ready
			b := make([]byte, 1)
			if _, err := r.Read(b); err != nil && err != io.EOF {
				return nil, fmt.Errorf("failed to read from logging binary: %w", err)
			}

			stdoutWriters = append(stdoutWriters, stdoutw)
			stderrWriters = append(stderrWriters, stderrw)
		}

		streams.Stdout = io.MultiWriter(stdoutWriters...)
		streams.Stderr = io.MultiWriter(stderrWriters...)

		if streams.FIFODir == "" {
			streams.FIFODir = defaults.DefaultFIFODir
		}
		fifos, err := cio.NewFIFOSetInDir(streams.FIFODir, id, streams.Terminal)
		if err != nil {
			return nil, err
		}

		if streams.Stdin == nil {
			fifos.Stdin = ""
		}
		if streams.Stdout == nil {
			fifos.Stdout = ""
		}
		if streams.Stderr == nil {
			fifos.Stderr = ""
		}
		return copyIO(cmd, fifos, streams)
	}
}

// NewMultiSessionContainerIO creates a container IO creator that supports multi-session IO sharing.
// This enables multiple clients (like start -a and attach) to share the same container's IO streams.
func NewMultiSessionContainerIO(containerID, namespace string, logURI string, tty bool, stdin io.Reader, stdout, stderr io.Writer) cio.Creator {
	return func(id string) (_ cio.IO, err error) {
		manager := streamutil.GetGlobalManager()

		// Get or create the stream config for this container
		streamConfig := manager.GetOrCreateStreamConfig(containerID, namespace, tty)

		// Attach this client to the stream config
		clientStreams, err := streamConfig.AttachClient(stdin, stdout, stderr)
		if err != nil {
			return nil, fmt.Errorf("failed to attach client to stream config: %w", err)
		}

		// Create the standard container IO for containerd that will be bridged with our stream config
		// We pass nil for stdin/stdout/stderr since they will be handled by the stream config
		standardIO, err := NewContainerIO(namespace, logURI, tty, nil, nil, nil)(id)
		if err != nil {
			clientStreams.Close()
			return nil, fmt.Errorf("failed to create standard container IO: %w", err)
		}

		// Create a wrapper that manages both the standard IO and the multi-session streams
		wrapper := &MultiSessionIOWrapper{
			standardIO:    standardIO,
			streamConfig:  streamConfig,
			clientStreams: clientStreams,
			containerID:   containerID,
		}

		return wrapper, nil
	}
}

// MultiSessionIOWrapper wraps standard container IO and adds multi-session support
type MultiSessionIOWrapper struct {
	standardIO    cio.IO
	streamConfig  *streamutil.StreamConfig
	clientStreams *streamutil.ClientStreams
	containerID   string
	bridgeOnce    sync.Once
}

// Config returns the IO configuration
func (w *MultiSessionIOWrapper) Config() cio.Config {
	return w.standardIO.Config()
}

// Cancel cancels the IO operations
func (w *MultiSessionIOWrapper) Cancel() {
	w.standardIO.Cancel()
}

// Wait waits for IO operations to complete
func (w *MultiSessionIOWrapper) Wait() {
	// Establish bridge on first Wait call (when DirectIO is ready)
	w.bridgeOnce.Do(func() {
		w.establishBridge()
	})

	w.standardIO.Wait()
}

// Close closes the IO and cleans up multi-session resources
func (w *MultiSessionIOWrapper) Close() error {
	var lastErr error

	// Close client streams first
	if err := w.clientStreams.Close(); err != nil {
		lastErr = err
	}

	// Close standard IO
	if err := w.standardIO.Close(); err != nil {
		lastErr = err
	}

	return lastErr
}

// establishBridge creates the bridge between containerd DirectIO and our StreamConfig
func (w *MultiSessionIOWrapper) establishBridge() {
	// Try to extract DirectIO from the standard IO wrapper
	if directIOAccess, ok := w.standardIO.(interface {
		DirectIO() *cio.DirectIO
	}); ok {
		if directIO := directIOAccess.DirectIO(); directIO != nil {
			// Establish the bridge with DirectIO
			w.streamConfig.CopyToPipe(directIO)
		}
	}
}
