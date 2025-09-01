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
	"testing"

	"github.com/containerd/nerdctl/mod/tigron/test"

	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest"
)

func TestCheckpointCreateErrors(t *testing.T) {
	testCase := nerdtest.Setup()
	testCase.SubTests = []*test.Case{
		{
			Description: "too-few-arguments",
			Command:     test.Command("checkpoint", "create", "too-few-arguments"),
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					ExitCode: 1,
				}
			},
		},
		{
			Description: "too-many-arguments",
			Command:     test.Command("checkpoint", "create", "too", "many", "arguments"),
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					ExitCode: 1,
				}
			},
		},
	}

	testCase.Run(t)
}
