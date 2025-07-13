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

package taskutil

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/containerd/defaults"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/nerdctl/v2/pkg/cioutil"
	"github.com/containerd/nerdctl/v2/pkg/streamutil"
	"github.com/pkg/errors"
)

// NewTask is from https://github.com/containerd/containerd/blob/v1.4.3/cmd/ctr/commands/tasks/tasks_unix.go#L70-L108
func NewTask(ctx context.Context, client *containerd.Client, container containerd.Container,
	attachStreamOpt []string, isInteractive, isTerminal, isDetach bool, logURI, detachKeys, namespace string, detachC chan<- struct{}) (containerd.Task, error) {

	var (
		t              containerd.Task
		rio            cio.IO
		stdinCloseSync = make(chan containerd.Process, 1)
		err            error
	)
	containerID := container.ID()
	t, err = container.NewTask(ctx, func(id string) (cio.IO, error) {
		fifos := cioutil.NewFIFOSet(defaults.DefaultFIFODir, id, isInteractive || isTerminal, isTerminal)

		rio, err = cioutil.CreateIO(containerID, fifos, stdinCloseSync, streamutil.GetGlobalIOWrapper().InitializeStdio)
		return rio, err
	})
	if err != nil {
		close(stdinCloseSync)
		if rio != nil {
			rio.Cancel()
			rio.Close()
		}
		return nil, errors.Wrap(err, "failed to create task for container")
	}

	// Signal c.createIO that it can call CloseIO
	stdinCloseSync <- t
	return t, nil
}

// struct used to store streams specified with attachStreamOpt (-a, --attach)
type streams struct {
	stdIn  *os.File
	stdOut *os.File
	stdErr *os.File
}

func nullStream() *os.File {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil
	}
	defer devNull.Close()

	return devNull
}

func processAttachStreamsOpt(streamsArr []string) streams {
	stdIn := os.Stdin
	stdOut := os.Stdout
	stdErr := os.Stderr

	for i, str := range streamsArr {
		streamsArr[i] = strings.ToUpper(str)
	}

	if !slices.Contains(streamsArr, "STDIN") {
		stdIn = nullStream()
	}

	if !slices.Contains(streamsArr, "STDOUT") {
		stdOut = nullStream()
	}

	if !slices.Contains(streamsArr, "STDERR") {
		stdErr = nullStream()
	}

	return streams{
		stdIn:  stdIn,
		stdOut: stdOut,
		stdErr: stdErr,
	}
}

// StdinCloser is from https://github.com/containerd/containerd/blob/v1.4.3/cmd/ctr/commands/tasks/exec.go#L181-L194
type StdinCloser struct {
	mu     sync.Mutex
	Stdin  *os.File
	Closer func()
	closed bool
}

func (s *StdinCloser) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, syscall.EBADF
	}
	n, err := s.Stdin.Read(p)
	if err != nil {
		if s.Closer != nil {
			s.Closer()
			s.closed = true
		}
	}
	return n, err
}

// Close implements Closer
func (s *StdinCloser) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.Closer != nil {
		s.Closer()
	}
	s.closed = true
	return nil
}
