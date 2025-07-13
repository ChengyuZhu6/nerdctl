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

package container

import (
	"context"
	"fmt"
	"io"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/containerutil"
	"github.com/containerd/nerdctl/v2/pkg/idutil/containerwalker"
	"github.com/containerd/nerdctl/v2/pkg/labels"
	"github.com/containerd/nerdctl/v2/pkg/streamutil"
)

// toReadCloser safely converts io.Reader to io.ReadCloser
func toReadCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

// Attach attaches stdin, stdout, and stderr to a running container.
func Attach(ctx context.Context, client *containerd.Client, req string, options types.ContainerAttachOptions) error {
	// Find the container.
	var container containerd.Container
	var cStatus containerd.Status

	walker := &containerwalker.ContainerWalker{
		Client: client,
		OnFound: func(ctx context.Context, found containerwalker.Found) error {
			container = found.Container
			return nil
		},
	}
	n, err := walker.Walk(ctx, req)
	if err != nil {
		return fmt.Errorf("error when trying to find the container: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no container is found given the string: %s", req)
	} else if n > 1 {
		return fmt.Errorf("more than one containers are found given the string: %s", req)
	}

	defer func() {
		containerLabels, err := container.Labels(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to getting container labels: %s", err)
			return
		}
		rm, err := containerutil.DecodeContainerRmOptLabel(containerLabels[labels.ContainerAutoRemove])
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to decode string to bool value: %s", err)
			return
		}
		if rm && cStatus.Status == containerd.Stopped {
			if err = RemoveContainer(ctx, container, options.GOptions, true, true, client); err != nil {
				log.L.WithError(err).Warnf("failed to remove container %s: %s", req, err)
			}
		}
	}()

	// Attach to the container.
	spec, err := container.Spec(ctx)
	if err != nil {
		return fmt.Errorf("failed to get the OCI runtime spec for the container: %w", err)
	}

	containerID := container.ID()
	containerIOWrapper := streamutil.GetGlobalIOWrapper()

	cfg := streamutil.AttachConfig{
		UseStdin:   options.Stdin != nil,
		UseStdout:  options.Stdout != nil,
		UseStderr:  options.Stderr != nil,
		TTY:        spec.Process.Terminal,
		CloseStdin: true,
		DetachKeys: []byte(options.DetachKeys),
	}

	containerIOWrapper.AttachStreams(containerID, &cfg)

	clientCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		<-clientCtx.Done()
		// The client has disconnected
		// In this case we need to close the container's output streams so that the goroutines used to copy
		// to the client streams are unblocked and can exit.
		if cfg.CStdout != nil {
			cfg.CStdout.Close()
		}
		if cfg.CStderr != nil {
			cfg.CStderr.Close()
		}
		containerLabels, err := container.Labels(ctx)
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to getting container labels: %s", err)
			return
		}
		rm, err := containerutil.DecodeContainerRmOptLabel(containerLabels[labels.ContainerAutoRemove])
		if rm && cStatus.Status == containerd.Stopped {
			RemoveContainer(ctx, container, options.GOptions, true, true, client)
		}
	}()

	if cfg.UseStdin {
		cfg.Stdin = toReadCloser(options.Stdin)
	}
	if cfg.UseStdout {
		cfg.Stdout = options.Stdout
	}
	if cfg.UseStderr {
		cfg.Stderr = options.Stderr
	}

	if cfg.Stdin != nil {
		r, w := io.Pipe()
		go func(stdin io.ReadCloser) {
			io.Copy(w, stdin)
			log.G(context.TODO()).WithFields(log.Fields{
				"container": containerID,
			}).Debug("Closing buffered stdin pipe")
			w.Close()
		}(cfg.Stdin)
		cfg.Stdin = r
	}

	err = <-containerIOWrapper.CopyStreams(containerID, &cfg)
	if err != nil {
		return fmt.Errorf("failed to copy streams: %w", err)
	}

	return nil
}
