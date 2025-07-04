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
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
)

// ContainerIOManager manages IO streams for all containers.
// It implements Docker's global container IO management pattern.
type ContainerIOManager struct {
	mu      sync.RWMutex
	streams map[string]*StreamConfig // containerID -> StreamConfig
}

var (
	globalManager     *ContainerIOManager
	globalManagerOnce sync.Once
)

// GetGlobalManager returns the global container IO manager instance
func GetGlobalManager() *ContainerIOManager {
	globalManagerOnce.Do(func() {
		globalManager = &ContainerIOManager{
			streams: make(map[string]*StreamConfig),
		}
	})
	return globalManager
}

// GetOrCreateStreamConfig gets or creates a StreamConfig for a container
func (m *ContainerIOManager) GetOrCreateStreamConfig(containerID, namespace string, tty bool) *StreamConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	sc, exists := m.streams[containerID]
	if !exists {
		sc = NewStreamConfig(containerID, namespace, tty)
		m.streams[containerID] = sc
		log.L.Debugf("Created new StreamConfig for container %s", containerID)
	}

	return sc
}

// GetStreamConfig gets an existing StreamConfig for a container
func (m *ContainerIOManager) GetStreamConfig(containerID string) (*StreamConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sc, exists := m.streams[containerID]
	return sc, exists
}

// RemoveStreamConfig removes a StreamConfig for a container
func (m *ContainerIOManager) RemoveStreamConfig(containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sc, exists := m.streams[containerID]
	if !exists {
		return nil
	}

	// Close the stream config
	if err := sc.Close(); err != nil {
		log.L.Warnf("Error closing StreamConfig for container %s: %v", containerID, err)
	}

	delete(m.streams, containerID)
	log.L.Debugf("Removed StreamConfig for container %s", containerID)
	return nil
}

// AttachToContainer attaches client streams to a container's IO
func (m *ContainerIOManager) AttachToContainer(containerID, namespace string, tty bool, stdin io.Reader, stdout, stderr io.Writer) (*ClientStreams, error) {
	sc := m.GetOrCreateStreamConfig(containerID, namespace, tty)
	return sc.AttachClient(stdin, stdout, stderr)
}

// CreateIOCreator creates a cio.Creator that integrates with the stream management system
func (m *ContainerIOManager) CreateIOCreator(containerID, namespace string, tty bool) cio.Creator {
	return func(id string) (cio.IO, error) {
		sc := m.GetOrCreateStreamConfig(containerID, namespace, tty)

		// Create a DirectIOWrapper that bridges containerd DirectIO with our StreamConfig
		wrapper := &DirectIOWrapper{
			streamConfig: sc,
			containerID:  containerID,
		}

		return wrapper, nil
	}
}

// ListActiveContainers returns a list of containers that have active stream configs
func (m *ContainerIOManager) ListActiveContainers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	containers := make([]string, 0, len(m.streams))
	for containerID := range m.streams {
		containers = append(containers, containerID)
	}
	return containers
}

// GetActiveStreamCount returns the total number of active stream configs
func (m *ContainerIOManager) GetActiveStreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

// DirectIOWrapper wraps a StreamConfig to implement cio.IO interface
type DirectIOWrapper struct {
	streamConfig *StreamConfig
	containerID  string
	directIO     *cio.DirectIO
}

// Config returns the cio configuration
func (w *DirectIOWrapper) Config() cio.Config {
	if w.directIO != nil {
		return w.directIO.Config()
	}
	return cio.Config{}
}

// Cancel cancels the IO operations
func (w *DirectIOWrapper) Cancel() {
	if w.directIO != nil {
		w.directIO.Cancel()
	}
}

// Wait waits for IO operations to complete
func (w *DirectIOWrapper) Wait() {
	if w.directIO != nil {
		w.directIO.Wait()
	}
	w.streamConfig.Wait()
}

// Close closes the IO
func (w *DirectIOWrapper) Close() error {
	var err error
	if w.directIO != nil {
		err = w.directIO.Close()
	}

	// Remove the stream config from the global manager when the container stops
	GetGlobalManager().RemoveStreamConfig(w.containerID)

	return err
}

// SetDirectIO sets the underlying DirectIO and establishes the bridge
func (w *DirectIOWrapper) SetDirectIO(dio *cio.DirectIO) {
	w.directIO = dio
	w.streamConfig.CopyToPipe(dio)
}

// StdinPipe returns the stdin pipe for writing
func (w *DirectIOWrapper) StdinPipe() io.WriteCloser {
	return w.streamConfig.StdinPipe()
}

// NewContainerIOCreator creates a cio.Creator for a container with multi-session support
func NewContainerIOCreator(containerID, namespace string, tty bool, logURI string) cio.Creator {
	return func(id string) (cio.IO, error) {
		manager := GetGlobalManager()
		sc := manager.GetOrCreateStreamConfig(containerID, namespace, tty)

		// Create wrapper
		wrapper := &DirectIOWrapper{
			streamConfig: sc,
			containerID:  containerID,
		}

		// Create the actual DirectIO for containerd
		if logURI != "" && logURI != "none" {
			// If we have a log URI, we need to create a more complex IO setup
			// For now, we'll use a simple approach
			directIO, err := createDirectIOWithLogging(id, logURI, tty)
			if err != nil {
				return nil, fmt.Errorf("failed to create DirectIO with logging: %w", err)
			}
			wrapper.SetDirectIO(directIO)
		} else {
			// For simple cases without logging
			directIO, err := createSimpleDirectIO(id, tty)
			if err != nil {
				return nil, fmt.Errorf("failed to create simple DirectIO: %w", err)
			}
			wrapper.SetDirectIO(directIO)
		}

		return wrapper, nil
	}
}

// createDirectIOWithLogging creates a DirectIO with logging support
func createDirectIOWithLogging(id, logURI string, tty bool) (*cio.DirectIO, error) {
	// This would need to be implemented based on nerdctl's existing logging infrastructure
	// For now, we'll create a simple DirectIO
	return createSimpleDirectIO(id, tty)
}

// createSimpleDirectIO creates a simple DirectIO without logging
func createSimpleDirectIO(id string, tty bool) (*cio.DirectIO, error) {
	// For now, return nil and let the existing cioutil handle DirectIO creation
	// This function will be properly implemented when integrating with the existing nerdctl infrastructure
	return nil, fmt.Errorf("DirectIO creation should be handled by existing cioutil infrastructure")
}
