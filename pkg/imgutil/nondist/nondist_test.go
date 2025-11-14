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

package nondist

import (
	"testing"
)

func TestIsNonDistributableMediaType(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		expected  bool
	}{
		{
			name:      "Docker foreign layer",
			mediaType: "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
			expected:  true,
		},
		{
			name:      "OCI non-distributable layer",
			mediaType: "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip",
			expected:  true,
		},
		{
			name:      "Regular Docker layer",
			mediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
			expected:  false,
		},
		{
			name:      "Regular OCI layer",
			mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			expected:  false,
		},
		{
			name:      "Docker manifest",
			mediaType: "application/vnd.docker.distribution.manifest.v2+json",
			expected:  false,
		},
		{
			name:      "OCI manifest",
			mediaType: "application/vnd.oci.image.manifest.v1+json",
			expected:  false,
		},
		{
			name:      "Config",
			mediaType: "application/vnd.docker.container.image.v1+json",
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsNonDistributableMediaType(tt.mediaType)
			if result != tt.expected {
				t.Errorf("IsNonDistributableMediaType(%q) = %v, want %v", tt.mediaType, result, tt.expected)
			}
		})
	}
}
