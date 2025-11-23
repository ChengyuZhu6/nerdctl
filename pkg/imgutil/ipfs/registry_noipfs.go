//go:build no_ipfs

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

package ipfs

import (
	"context"
	"fmt"
)

type Registry struct{}

func NewRegistry(ctx context.Context, ref, scheme, ipfsPath string) (*Registry, error) {
	return nil, fmt.Errorf("IPFS support is not enabled in this build")
}
