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

	"golang.org/x/term"

	"github.com/containerd/console"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/consoleutil"
	"github.com/containerd/nerdctl/v2/pkg/containerutil"
	"github.com/containerd/nerdctl/v2/pkg/errutil"
	"github.com/containerd/nerdctl/v2/pkg/idutil/containerwalker"
	"github.com/containerd/nerdctl/v2/pkg/labels"
	"github.com/containerd/nerdctl/v2/pkg/signalutil"
	"github.com/containerd/nerdctl/v2/pkg/streamutil"
)

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

	// Get container spec to determine TTY mode
	spec, err := container.Spec(ctx)
	if err != nil {
		return fmt.Errorf("failed to get the OCI runtime spec for the container: %w", err)
	}

	flagT := spec.Process.Terminal
	var con console.Console

	if flagT {
		con, err = consoleutil.Current()
		if err != nil {
			return fmt.Errorf("failed to get the current console: %w", err)
		}
		defer con.Reset()
		if _, err := term.MakeRaw(int(con.Fd())); err != nil {
			return fmt.Errorf("failed to set the console to raw mode: %w", err)
		}
	}

	// Get container labels for namespace
	containerLabels, err := container.Labels(ctx)
	if err != nil {
		return fmt.Errorf("failed to get container labels: %w", err)
	}
	namespace := containerLabels[labels.Namespace]

	// Use multi-session IO manager to attach to the container
	manager := streamutil.GetGlobalManager()

	// Prepare IO streams
	var stdin io.Reader
	var stdout, stderr io.Writer = options.Stdout, options.Stderr

	if options.Stdin != nil {
		if flagT {
			// For TTY mode, we need detachable stdin
			closer := func() {
				// This will be called when detach key sequence is detected
				log.G(ctx).Debug("Detaching from container")
			}
			stdin, err = consoleutil.NewDetachableStdin(con, options.DetachKeys, closer)
			if err != nil {
				return fmt.Errorf("failed to create detachable stdin: %w", err)
			}
		} else {
			stdin = options.Stdin
		}
	}

	if flagT {
		// For TTY mode, stdout and stderr are combined
		stdout = con
		stderr = nil
	}

	// Attach to the container's IO streams
	clientStreams, err := manager.AttachToContainer(container.ID(), namespace, flagT, stdin, stdout, stderr)
	if err != nil {
		return fmt.Errorf("failed to attach to container IO: %w", err)
	}
	defer clientStreams.Close()

	// Get the existing task (container should already be running)
	task, err := container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to get container task: %w", err)
	}
	if flagT {
		// Handle console resize for TTY mode
		if err := consoleutil.HandleConsoleResize(ctx, task, con); err != nil {
			log.G(ctx).WithError(err).Error("console resize")
		}
	}

	// Forward signals to the container
	sigC := signalutil.ForwardAllSignals(ctx, task)
	defer signalutil.StopCatch(sigC)

	// Wait for the container to exit or user to detach
	statusC, err := task.Wait(ctx)
	if err != nil {
		return fmt.Errorf("failed to init an async wait for the container to exit: %w", err)
	}

	// Since we're using multi-session IO, we don't have a detachC channel like in the original code
	// Instead, we'll wait for the container to exit
	status := <-statusC
	cStatus, err = task.Status(ctx)
	if err != nil {
		return err
	}
	code, _, err := status.Result()
	if err != nil {
		return err
	}
	if code != 0 {
		return errutil.NewExitCoderErr(int(code))
	}
	return nil
}
