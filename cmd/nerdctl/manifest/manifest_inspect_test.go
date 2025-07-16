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

package manifest

import (
	"encoding/json"
	"testing"

	"github.com/containerd/nerdctl/mod/tigron/test"
	"github.com/containerd/nerdctl/mod/tigron/tig"
	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest"
	"gotest.tools/v3/assert"
)

func TestManifestInspect(t *testing.T) {
	testCase := nerdtest.Setup()
	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("pull", "--all-platforms", "alpine:3.22.1")
	}
	testCase.SubTests = []*test.Case{
		{
			Description: "tag-non-verbose",
			Command:     test.Command("manifest", "inspect", "alpine:3.22.1"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				index, ok := arr[0]["Index"].(map[string]interface{})
				assert.Assert(t, ok)
				assert.Equal(t, int(index["schemaVersion"].(float64)), 2)
				manifests, ok := index["manifests"].([]interface{})
				assert.Assert(t, ok)
				assert.Assert(t, len(manifests) > 0)
			}),
		},
		{
			Description: "tag-verbose",
			Command:     test.Command("manifest", "inspect", "alpine:3.22.1", "--verbose"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				assert.Equal(t, arr[0]["ImageName"], "docker.io/library/alpine:3.22.1")
				indexDesc := arr[0]["IndexDesc"].(map[string]interface{})
				assert.Equal(t, indexDesc["mediaType"], "application/vnd.oci.image.index.v1+json")
				manifests, ok := arr[0]["Manifests"].([]interface{})
				assert.Assert(t, ok, "should have Manifests field")
				assert.Assert(t, len(manifests) > 0)
				manifestObj, ok := manifests[0].(map[string]interface{})
				assert.Assert(t, ok, "Manifests[0] should be object")
				manifestArr := []map[string]interface{}{manifestObj}
				assertManifestConfigDigest(t, manifestArr, "sha256:9234e8fb04c47cfe0f49931e4ac7eb76fa904e33b7f8576aec0501c085f02516")
				assertManifestDescDigest(t, manifestArr, "sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f")
			}),
		},
		{
			Description: "digest-non-verbose",
			Command:     test.Command("manifest", "inspect", "alpine@sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				assertManifestConfigDigest(t, arr, "sha256:9234e8fb04c47cfe0f49931e4ac7eb76fa904e33b7f8576aec0501c085f02516")
			}),
		},
		{
			Description: "digest-verbose",
			Command:     test.Command("manifest", "inspect", "alpine@sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f", "--verbose"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				assertManifestConfigDigest(t, arr, "sha256:9234e8fb04c47cfe0f49931e4ac7eb76fa904e33b7f8576aec0501c085f02516")
				assertManifestDescDigest(t, arr, "sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f")
			}),
		},
		{
			Description: "sha256-digest-non-verbose",
			Command:     test.Command("manifest", "inspect", "sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				assertManifestConfigDigest(t, arr, "sha256:9234e8fb04c47cfe0f49931e4ac7eb76fa904e33b7f8576aec0501c085f02516")
			}),
		},
		{
			Description: "sha256-digest-verbose",
			Command:     test.Command("manifest", "inspect", "sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f", "--verbose"),
			Expected: test.Expects(0, nil, func(stdout string, t tig.T) {
				var arr []map[string]interface{}
				assert.NilError(t, json.Unmarshal([]byte(stdout), &arr))
				assert.Assert(t, len(arr) > 0)
				assertManifestConfigDigest(t, arr, "sha256:9234e8fb04c47cfe0f49931e4ac7eb76fa904e33b7f8576aec0501c085f02516")
				assertManifestDescDigest(t, arr, "sha256:eafc1edb577d2e9b458664a15f23ea1c370214193226069eb22921169fc7e43f")
			}),
		},
	}

	testCase.Run(t)
}

func assertManifestConfigDigest(t tig.T, arr []map[string]interface{}, wantDigest string) {
	manifest, ok := arr[0]["Manifest"].(map[string]interface{})
	assert.Assert(t, ok)
	config, ok := manifest["config"].(map[string]interface{})
	assert.Assert(t, ok, "should have config field")
	assert.Equal(t, config["digest"], wantDigest)
}

func assertManifestDescDigest(t tig.T, arr []map[string]interface{}, wantDigest string) {
	manifestDesc, ok := arr[0]["ManifestDesc"].(map[string]interface{})
	assert.Assert(t, ok, "should have ManifestDesc field")
	assert.Equal(t, manifestDesc["digest"], wantDigest)
}
