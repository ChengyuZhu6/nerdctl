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

package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/archive"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/idutil/containerwalker"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
)

func Create(ctx context.Context, client *containerd.Client, containerID string, checkpointName string, options types.CheckpointCreateOptions) error {
	var container containerd.Container

	walker := &containerwalker.ContainerWalker{
		Client: client,
		OnFound: func(ctx context.Context, found containerwalker.Found) error {
			if found.MatchCount > 1 {
				return fmt.Errorf("multiple containers found with provided prefix: %s", found.Req)
			}
			container = found.Container
			return nil
		},
	}

	n, err := walker.Walk(ctx, containerID)
	if err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("error creating checkpoint for container: %s, no such container", containerID)
	}

	opts := []containerd.CheckpointOpts{}
	if !options.LeaveRunning {
		opts = append(opts, containerd.WithCheckpointTaskExit)
	} else {
		opts = append(opts, containerd.WithCheckpointTask)
	}

	img, err := container.Checkpoint(ctx, checkpointName, opts...)
	if err != nil {
		return err
	}

	defer func() {
		err := client.ImageService().Delete(ctx, img.Name())
		if err != nil {
			fmt.Fprintf(options.Stdout, "failed to delete checkpoint image: %v\n", err)
		}
	}()

	cs := client.ContentStore()

	rawIndex, err := content.ReadBlob(ctx, cs, img.Target())
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to retrieve checkpoint data"))
	}

	var index ocispec.Index
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to decode checkpoint data"))
	}

	var cpDesc *ocispec.Descriptor
	for _, m := range index.Manifests {
		if m.MediaType == images.MediaTypeContainerd1Checkpoint {
			cpDesc = &m //nolint:gosec
			break
		}
	}
	if cpDesc == nil {
		return errdefs.System(errors.Wrapf(err, "invalid checkpoint"))
	}

	targetPath := filepath.Join(options.CheckpointDir, checkpointName)
	if err := os.MkdirAll(targetPath, 0o700); err != nil {
		return err
	}

	rat, err := cs.ReaderAt(ctx, *cpDesc)
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to get checkpoint reader"))
	}
	defer rat.Close()

	_, err = archive.Apply(ctx, targetPath, content.NewReader(rat))
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "failed to read checkpoint reader"))
	}

	fmt.Fprintf(options.Stdout, "%s\n", checkpointName)
	return nil
}
