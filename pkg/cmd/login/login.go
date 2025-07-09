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

package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/net/context/ctxhttp"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdctl/v2/pkg/api/types"
	"github.com/containerd/nerdctl/v2/pkg/errutil"
	"github.com/containerd/nerdctl/v2/pkg/imgutil/dockerconfigresolver"
)

const unencryptedPasswordWarning = `WARNING: Your password will be stored unencrypted in %s.
Configure a credential helper to remove this warning. See
https://docs.docker.com/engine/reference/commandline/login/#credentials-store
`

func Login(ctx context.Context, options types.LoginCommandOptions, stdout io.Writer) error {
	registryURL, err := dockerconfigresolver.Parse(options.ServerAddress)
	if err != nil {
		return err
	}

	credStore, err := dockerconfigresolver.NewCredentialsStore("")
	if err != nil {
		return err
	}
	log.G(ctx).Debugf("credStore: %+v", credStore)
	var responseIdentityToken string
	log.G(ctx).Debugf("options.Username: %+v", options.Username)
	log.G(ctx).Debugf("options.Password: %+v", options.Password)
	credentials, err := credStore.Retrieve(registryURL, options.Username == "" && options.Password == "")
	credentials.IdentityToken = ""
	log.G(ctx).Debugf("credentials: %+v", credentials)
	if err == nil && credentials.Username != "" && credentials.Password != "" {
		responseIdentityToken, err = loginClientSide(ctx, options.GOptions, registryURL, credentials)
	}

	if err != nil || credentials.Username == "" || credentials.Password == "" {
		err = promptUserForAuthentication(credentials, options.Username, options.Password, stdout)
		if err != nil {
			return err
		}
		log.G(ctx).Debugf("after promptUserForAuthentication, credentials: %+v", credentials)
		responseIdentityToken, err = loginClientSide(ctx, options.GOptions, registryURL, credentials)
		if err != nil {
			return err
		}
	}

	if responseIdentityToken != "" {
		credentials.Password = ""
		credentials.IdentityToken = responseIdentityToken
	}

	// Display a warning if we're storing the users password (not a token) and credentials store type is file.
	storageFileLocation := credStore.FileStorageLocation(registryURL)
	if storageFileLocation != "" && credentials.Password != "" {
		_, err = fmt.Fprintln(stdout, fmt.Sprintf(unencryptedPasswordWarning, storageFileLocation))
		if err != nil {
			return err
		}
	}

	err = credStore.Store(registryURL, credentials)
	if err != nil {
		return fmt.Errorf("error saving credentials: %w", err)
	}

	// When the port is the https default (443), other clients cannot be expected to necessarily lookup the variants with port
	// so save it both with and without port.
	// This is the case for at least buildctl: https://github.com/containerd/nerdctl/issues/3748
	if registryURL.Port() == dockerconfigresolver.StandardHTTPSPort {
		registryURL.Host = registryURL.Hostname()
		err = credStore.Store(registryURL, credentials)
		if err != nil {
			return fmt.Errorf("error saving credentials: %w", err)
		}
	}

	_, err = fmt.Fprintln(stdout, "Login Succeeded")

	return err
}

func loginClientSide(ctx context.Context, globalOptions types.GlobalCommandOptions, registryURL *dockerconfigresolver.RegistryURL, credentials *dockerconfigresolver.Credentials) (string, error) {
	host := registryURL.Host
	var dOpts []dockerconfigresolver.Opt
	if globalOptions.InsecureRegistry {
		log.G(ctx).Warnf("skipping verifying HTTPS certs for %q", host)
		dOpts = append(dOpts, dockerconfigresolver.WithSkipVerifyCerts(true))
	}
	dOpts = append(dOpts, dockerconfigresolver.WithHostsDirs(globalOptions.HostsDir))

	authCreds := func(acArg string) (string, string, error) {
		if acArg == host {
			if credentials.RegistryToken != "" {
				// Even containerd/CRI does not support RegistryToken as of v1.4.3,
				// so, nobody is actually using RegistryToken?
				log.G(ctx).Warnf("RegistryToken (for %q) is not supported yet (FIXME)", host)
			}
			return credentials.Username, credentials.Password, nil
		}
		return "", "", fmt.Errorf("expected acArg to be %q, got %q", host, acArg)
	}

	dOpts = append(dOpts, dockerconfigresolver.WithAuthCreds(authCreds))
	ho, err := dockerconfigresolver.NewHostOptions(ctx, host, dOpts...)
	if err != nil {
		return "", err
	}
	fetchedRefreshTokens := make(map[string]string) // key: req.URL.Host
	// onFetchRefreshToken is called when tryLoginWithRegHost calls rh.Authorizer.Authorize()
	onFetchRefreshToken := func(ctx context.Context, s string, req *http.Request) {
		fetchedRefreshTokens[req.URL.Host] = s
	}
	ho.AuthorizerOpts = append(ho.AuthorizerOpts, docker.WithFetchRefreshToken(onFetchRefreshToken))
	regHosts, err := config.ConfigureHosts(ctx, *ho)(host)
	if err != nil {
		return "", err
	}
	log.G(ctx).Debugf("len(regHosts)=%d", len(regHosts))
	log.G(ctx).Debugf("regHosts: %+v", regHosts)
	if len(regHosts) == 0 {
		return "", fmt.Errorf("got empty []docker.RegistryHost for %q", host)
	}
	for i, rh := range regHosts {
		err = tryLoginWithRegHost(ctx, rh)
		if err != nil && globalOptions.InsecureRegistry && (errors.Is(err, http.ErrSchemeMismatch) || errutil.IsErrConnectionRefused(err)) {
			rh.Scheme = "http"
			err = tryLoginWithRegHost(ctx, rh)
		}
		identityToken := fetchedRefreshTokens[rh.Host] // can be empty
		if err == nil {
			return identityToken, nil
		}
		log.G(ctx).WithError(err).WithField("i", i).Error("failed to call tryLoginWithRegHost")
	}
	return "", err
}

func tryLoginWithRegHost(ctx context.Context, rh docker.RegistryHost) error {
	if rh.Authorizer == nil {
		return errors.New("got nil Authorizer")
	}
	if rh.Path == "/v2" {
		// If the path is using /v2 endpoint but lacks trailing slash add it
		// https://docs.docker.com/registry/spec/api/#detail. Acts as a workaround
		// for containerd issue https://github.com/containerd/containerd/blob/2986d5b077feb8252d5d2060277a9c98ff8e009b/remotes/docker/config/hosts.go#L110
		rh.Path = "/v2/"
	}
	u := url.URL{
		Scheme: rh.Scheme,
		Host:   rh.Host,
		Path:   rh.Path,
	}
	log.G(ctx).Debugf("u: %+v", u)
	log.G(ctx).Debugf("rh.Authorizer: %#v", rh.Authorizer)

	// Wrap the transport once to dump full HTTP request / response for debugging
	if rh.Client != nil {
		if _, wrapped := rh.Client.Transport.(roundTripperDebug); !wrapped {
			baseRT := rh.Client.Transport
			if baseRT == nil {
				baseRT = http.DefaultTransport
			}
			rh.Client.Transport = roundTripperDebug{base: baseRT}
		}
	}
	var ress []*http.Response
	for i := 0; i < 10; i++ {
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		for k, v := range rh.Header.Clone() {
			for _, vv := range v {
				req.Header.Add(k, vv)
			}
		}
		if err := rh.Authorizer.Authorize(ctx, req); err != nil {
			return fmt.Errorf("failed to call rh.Authorizer.Authorize: %w", err)
		}
		res, err := ctxhttp.Do(ctx, rh.Client, req)
		if err != nil {
			return fmt.Errorf("failed to call rh.Client.Do: %w", err)
		}
		ress = append(ress, res)
		if res.StatusCode == 401 {
			if err := rh.Authorizer.AddResponses(ctx, ress); err != nil && !errdefs.IsNotImplemented(err) {
				return fmt.Errorf("failed to call rh.Authorizer.AddResponses: %w", err)
			}
			continue
		}
		if res.StatusCode/100 != 2 {
			return fmt.Errorf("unexpected status code %d", res.StatusCode)
		}

		return nil
	}

	return errors.New("too many 401 (probably)")
}

// roundTripperDebug is a wrapper around http.RoundTripper that logs
// the complete request & response metadata (and first 512 bytes of body)
// to nerdctl's debug logger. It is only enabled when --debug or --debug-full
// is active.
type roundTripperDebug struct{ base http.RoundTripper }

func (rt roundTripperDebug) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	log.G(ctx).Debugf("[HTTP-TRACE] --> %s %s", req.Method, req.URL)
	for k, v := range req.Header {
		log.G(ctx).Debugf("[HTTP-TRACE] --> %s: %q", k, v)
	}

	res, err := rt.base.RoundTrip(req)
	if err != nil {
		log.G(ctx).WithError(err).Debug("[HTTP-TRACE] <-- transport error")
		return res, err
	}

	log.G(ctx).Debugf("[HTTP-TRACE] <-- %s %s", res.Proto, res.Status)
	for k, v := range res.Header {
		log.G(ctx).Debugf("[HTTP-TRACE] <-- %s: %q", k, v)
	}

	if res.Body != nil {
		// Peek first 512 bytes for debugging while keeping body consumable
		var previewBuf bytes.Buffer
		if _, _ = io.CopyN(&previewBuf, res.Body, 512); previewBuf.Len() > 0 {
			log.G(ctx).Debugf("[HTTP-TRACE] <-- first 512B body: %q", previewBuf.String())
		}
		res.Body = io.NopCloser(io.MultiReader(bytes.NewReader(previewBuf.Bytes()), res.Body))
	}
	return res, nil
}
