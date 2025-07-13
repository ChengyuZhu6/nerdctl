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

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
)

type rio struct {
	cio.IO
	sc *StreamConfig
}

func (i *rio) Close() error {
	i.IO.Close()
	return i.sc.CloseStreams()
}

func (i *rio) Wait() {
	i.sc.Wait(context.Background())
	i.IO.Wait()
}

type aio struct {
	cio.IO

	wg sync.WaitGroup
}

func (c *aio) Wait() {
	c.wg.Wait()
	c.IO.Wait()
}

// ContainerIOWrapper manages IO streams for all containers.
type ContainerIOWrapper struct {
	mu      sync.RWMutex
	streams map[string]*StreamConfig // containerID -> StreamConfig
}

var (
	globalIOWrapper     *ContainerIOWrapper
	globalIOWrapperOnce sync.Once
)

// GetGlobalIOWrapper returns the global container IO wrapper instance
func GetGlobalIOWrapper() *ContainerIOWrapper {
	globalIOWrapperOnce.Do(func() {
		globalIOWrapper = &ContainerIOWrapper{
			streams: make(map[string]*StreamConfig),
		}
	})
	return globalIOWrapper
}

func (m *ContainerIOWrapper) NewStreamConfig(containerID string) *StreamConfig {
	m.streams[containerID] = NewStreamConfig()
	return m.streams[containerID]
}

func (m *ContainerIOWrapper) NewInputPipes(containerID string, isTerminal bool) {
	if isTerminal {
		m.streams[containerID].NewInputPipes()
	} else {
		m.streams[containerID].NewNopInputPipe()
	}
}

// StdinPipe gets the stdin stream of the container
func (m *ContainerIOWrapper) StdinPipe(containerID string) io.WriteCloser {
	return m.streams[containerID].StdinPipe()
}

// StdoutPipe gets the stdout stream of the container
func (m *ContainerIOWrapper) StdoutPipe(containerID string) io.ReadCloser {
	return m.streams[containerID].StdoutPipe()
}

// StderrPipe gets the stderr stream of the container
func (m *ContainerIOWrapper) StderrPipe(containerID string) io.ReadCloser {
	return m.streams[containerID].StderrPipe()
}

// CloseStreams closes the container's stdio streams
func (m *ContainerIOWrapper) CloseStreams(containerID string) error {
	return m.streams[containerID].CloseStreams()
}

// AttachStreams attaches the container's streams to the AttachConfig
func (m *ContainerIOWrapper) AttachStreams(containerID string, cfg *AttachConfig) {
	m.streams[containerID].AttachStreams(cfg)
}

// CopyStreams copies the container's streams to the AttachConfig
func (m *ContainerIOWrapper) CopyStreams(containerID string, cfg *AttachConfig) <-chan error {
	return m.streams[containerID].CopyStreams(context.Background(), cfg)
}

// InitializeStdio is called by libcontainerd to connect the stdio.
func (m *ContainerIOWrapper) InitializeStdio(containerID string, iop *cio.DirectIO) (cio.IO, error) {
	m.streams[containerID].CopyToPipe(iop)

	if m.streams[containerID].Stdin() == nil {
		if iop.Stdin != nil {
			if err := iop.Stdin.Close(); err != nil {
				log.G(context.TODO()).Warnf("error closing stdin: %+v", err)
			}
		}
	}

	return &rio{IO: iop, sc: m.streams[containerID]}, nil
}

type StdioCallback func(containerID string, iop *cio.DirectIO) (cio.IO, error)

func attachStreamsFunc(stdout, stderr io.WriteCloser) StdioCallback {
	return func(containerID string, iop *cio.DirectIO) (cio.IO, error) {
		if iop.Stdin != nil {
			iop.Stdin.Close()
			// closing stdin shouldn't be needed here, it should never be open
			panic("plugin stdin shouldn't have been created!")
		}

		aio := &aio{IO: iop}
		aio.wg.Add(2)
		go func() {
			io.Copy(stdout, iop.Stdout)
			stdout.Close()
			aio.wg.Done()
		}()
		go func() {
			io.Copy(stderr, iop.Stderr)
			stderr.Close()
			aio.wg.Done()
		}()
		return aio, nil
	}
}
