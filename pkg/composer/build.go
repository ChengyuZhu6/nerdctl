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

package composer

import (
	"context"
	"fmt"
	"os"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/composer/serviceparser"
)

type BuildOptions struct {
	Args     []string // --build-arg strings
	NoCache  bool
	Progress string
}

func (c *Composer) Build(ctx context.Context, bo BuildOptions, services []string) error {
	return c.project.ForEachService(services, func(names string, svc *types.ServiceConfig) error {
		ps, err := serviceparser.Parse(c.project, *svc)
		if err != nil {
			return err
		}
		if ps.Build != nil {
			return c.buildServiceImage(ctx, ps.Image, ps.Build, ps.Unparsed.Platform, bo)
		}
		return nil
	}, types.IgnoreDependencies)
}

func (c *Composer) buildServiceImage(ctx context.Context, image string, b *serviceparser.Build, platform string, bo BuildOptions) error {
	log.G(ctx).Infof("Building image %s", image)

	var args []string // nolint: prealloc
	if platform != "" {
		args = append(args, "--platform="+platform)
	}
	for _, a := range bo.Args {
		args = append(args, "--build-arg="+a)
	}
	if bo.NoCache {
		args = append(args, "--no-cache")
	}
	if bo.Progress != "" {
		args = append(args, "--progress="+bo.Progress)
	}

	if b.DockerfileInline != "" {
		// if DockerfileInline is specified, write it to a temporary file
		// and use -f flag to use that docker file with project's ctxdir
		tmpFile, err := os.CreateTemp("", "inline-dockerfile-*.Dockerfile")
		if err != nil {
			return fmt.Errorf("failed to create temp file for DockerfileInline: %w", err)
		}
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()

		if _, err := tmpFile.Write([]byte(b.DockerfileInline)); err != nil {
			return fmt.Errorf("failed to write DockerfileInline: %w", err)
		}
		b.BuildArgs = append(b.BuildArgs, "-f="+tmpFile.Name())
	}

	args = append(args, b.BuildArgs...)

	cmd := c.createNerdctlCmd(ctx, append([]string{"build"}, args...)...)
	if c.DebugPrintFull {
		log.G(ctx).Debugf("Running %v", cmd.Args)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error while building image %s: %w", image, err)
	}
	return nil
}
