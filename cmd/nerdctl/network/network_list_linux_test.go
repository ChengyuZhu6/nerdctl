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

package network

import (
	"strings"
	"testing"

	"gotest.tools/v3/assert"

	"github.com/containerd/nerdctl/mod/tigron/test"
	"github.com/containerd/nerdctl/mod/tigron/tig"

	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest"
)

func TestNetworkLsFilter(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		data.Labels().Set("identifier", data.Identifier())
		data.Labels().Set("label", "mylabel=label-1")
		data.Labels().Set("net1", data.Identifier("1"))
		data.Labels().Set("net2", data.Identifier("2"))
		data.Labels().Set("netID1", helpers.Capture("network", "create", "--label="+data.Labels().Get("label"), data.Labels().Get("net1")))
		data.Labels().Set("netID2", helpers.Capture("network", "create", data.Labels().Get("net2")))
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("network", "rm", data.Identifier("1"))
		helpers.Anyhow("network", "rm", data.Identifier("2"))
	}

	testCase.SubTests = []*test.Case{
		{
			Description: "filter label",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				return helpers.Command("network", "ls", "--quiet", "--filter", "label="+data.Labels().Get("label"))
			},
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					Output: func(stdout string, t tig.T) {
						var lines = strings.Split(strings.TrimSpace(stdout), "\n")
						assert.Assert(t, len(lines) >= 1, "expected at least one line\n")
						netNames := map[string]struct{}{
							data.Labels().Get("netID1")[:12]: {},
						}

						for _, name := range lines {
							_, ok := netNames[name]
							assert.Assert(t, ok, "expected to find name\n")
						}
					},
				}
			},
		},
		{
			Description: "filter name",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				return helpers.Command("network", "ls", "--quiet", "--filter", "name="+data.Labels().Get("net2"))
			},
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					Output: func(stdout string, t tig.T) {
						var lines = strings.Split(strings.TrimSpace(stdout), "\n")
						assert.Assert(t, len(lines) >= 1, "expected at least one line\n")
						netNames := map[string]struct{}{
							data.Labels().Get("netID2")[:12]: {},
						}

						for _, name := range lines {
							_, ok := netNames[name]
							assert.Assert(t, ok, "expected to find name\n")
						}
					},
				}
			},
		},
		{
			Description: "filter name regexp",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				return helpers.Command("network", "ls", "--quiet", "--filter", "name=.*"+data.Labels().Get("net2")+".*")
			},
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					Output: func(stdout string, t tig.T) {
						var lines = strings.Split(strings.TrimSpace(stdout), "\n")
						assert.Assert(t, len(lines) >= 1)
						netNames := map[string]struct{}{
							data.Labels().Get("netID2")[:12]: {},
						}

						for _, name := range lines {
							_, ok := netNames[name]
							assert.Assert(t, ok)
						}
					},
				}
			},
		},
	}

	testCase.Run(t)
}
