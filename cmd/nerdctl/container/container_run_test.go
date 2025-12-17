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
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"

	"github.com/containerd/nerdctl/mod/tigron/expect"
	"github.com/containerd/nerdctl/mod/tigron/require"
	"github.com/containerd/nerdctl/mod/tigron/test"
	"github.com/containerd/nerdctl/mod/tigron/tig"

	"github.com/containerd/nerdctl/v2/pkg/rootlessutil"
	"github.com/containerd/nerdctl/v2/pkg/testutil"
	"github.com/containerd/nerdctl/v2/pkg/testutil/nerdtest"
)

func TestRunEntrypointWithBuild(t *testing.T) {
	nerdtest.Setup()

	dockerfile := fmt.Sprintf(`FROM %s
ENTRYPOINT ["echo", "foo"]
CMD ["echo", "bar"]
	`, testutil.CommonImage)

	testCase := &test.Case{
		Require: nerdtest.Build,
		Setup: func(data test.Data, helpers test.Helpers) {
			data.Temp().Save(dockerfile, "Dockerfile")
			data.Labels().Set("image", data.Identifier())
			helpers.Ensure("build", "-t", data.Labels().Get("image"), data.Temp().Path())
		},
		SubTests: []*test.Case{
			{
				Description: "Run image",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "--rm", data.Labels().Get("image"))
				},
				Expected: test.Expects(expect.ExitCodeSuccess, nil, expect.Equals("foo echo bar\n")),
			},
			{
				Description: "Run image empty entrypoint",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "--rm", "--entrypoint", "", data.Labels().Get("image"))
				},
				Expected: test.Expects(expect.ExitCodeGenericFail, nil, nil),
			},
			{
				Description: "Run image time entrypoint",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "--rm", "--entrypoint", "time", data.Labels().Get("image"))
				},
				Expected: test.Expects(expect.ExitCodeGenericFail, nil, nil),
			},
			{
				Description: "Run image empty entrypoint custom command",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "--rm", "--entrypoint", "", data.Labels().Get("image"), "echo", "blah")
				},
				Expected: test.Expects(expect.ExitCodeSuccess, nil, expect.All(
					expect.Contains("blah"),
					expect.DoesNotContain("foo", "bar"),
				)),
			},
			{
				Description: "Run image time entrypoint custom command",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "--rm", "--entrypoint", "time", data.Labels().Get("image"), "echo", "blah")
				},
				Expected: test.Expects(expect.ExitCodeSuccess, nil, expect.All(
					expect.Contains("blah"),
					expect.DoesNotContain("foo", "bar"),
				)),
			},
		},
	}

	testCase.Run(t)
}

func TestRunWorkdir(t *testing.T) {
	testCase := nerdtest.Setup()

	dir := "/foo"
	if runtime.GOOS == "windows" {
		dir = "c:" + dir
	}

	testCase.Command = test.Command("run", "--rm", "--workdir="+dir, testutil.CommonImage, "pwd")

	testCase.Expected = test.Expects(expect.ExitCodeSuccess, nil, expect.Contains(dir))

	testCase.Run(t)
}

func TestRunWithDoubleDash(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.Not(nerdtest.Docker)

	testCase.Command = test.Command("run", "--rm", testutil.CommonImage, "--", "sh", "-euxc", "exit 0")

	testCase.Expected = test.Expects(expect.ExitCodeSuccess, nil, nil)

	testCase.Run(t)
}

func TestRunExitCode(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rm", "-f", data.Identifier("exit0"))
		helpers.Anyhow("rm", "-f", data.Identifier("exit123"))
	}

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "--name", data.Identifier("exit0"), testutil.CommonImage, "sh", "-euxc", "exit 0")
		helpers.Command("run", "--name", data.Identifier("exit123"), testutil.CommonImage, "sh", "-euxc", "exit 123").
			Run(&test.Expected{ExitCode: 123})
	}

	testCase.Command = test.Command("ps", "-a")

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Errors:   nil,
			Output: expect.All(
				expect.Match(regexp.MustCompile("Exited [(]123[)][A-Za-z0-9 ]+"+data.Identifier("exit123"))),
				expect.Match(regexp.MustCompile("Exited [(]0[)][A-Za-z0-9 ]+"+data.Identifier("exit0"))),
				func(stdout string, t tig.T) {
					assert.Equal(t, nerdtest.InspectContainer(helpers, data.Identifier("exit0")).State.Status, "exited")
					assert.Equal(t, nerdtest.InspectContainer(helpers, data.Identifier("exit123")).State.Status, "exited")
				},
			),
		}
	}

	testCase.Run(t)
}

func TestRunCIDFile(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "--rm", "--cidfile", data.Temp().Path("cid-file"), testutil.CommonImage)
		data.Temp().Exists("cid-file")
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		return helpers.Command("run", "--rm", "--cidfile", data.Temp().Path("cid-file"), testutil.CommonImage)
	}

	// Docker will return 125 while nerdctl returns 1, so, generic fail instead of specific exit code
	testCase.Expected = test.Expects(expect.ExitCodeGenericFail, []error{errors.New("container ID file found")}, nil)

	testCase.Run(t)
}

func TestRunEnvFile(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Env = map[string]string{
		"HOST_ENV": "ENV-IN-HOST",
	}

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		data.Temp().Save("# this is a comment line\nTESTKEY1=TESTVAL1", "env1-file")
		data.Temp().Save("# this is a comment line\nTESTKEY2=TESTVAL2\nHOST_ENV", "env2-file")
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		return helpers.Command(
			"run", "--rm",
			"--env-file", data.Temp().Path("env1-file"),
			"--env-file", data.Temp().Path("env2-file"),
			testutil.CommonImage, "env")
	}

	testCase.Expected = test.Expects(
		expect.ExitCodeSuccess,
		nil,
		expect.Contains("TESTKEY1=TESTVAL1", "TESTKEY2=TESTVAL2", "HOST_ENV=ENV-IN-HOST"),
	)

	testCase.Run(t)
}

func TestRunEnv(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Env = map[string]string{
		"CORGE":  "corge-value-in-host",
		"GARPLY": "garply-value-in-host",
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		return helpers.Command("run", "--rm",
			"--env", "FOO=foo1,foo2",
			"--env", "BAR=bar1 bar2",
			"--env", "BAZ=",
			"--env", "QUX", // not exported in OS
			"--env", "QUUX=quux1",
			"--env", "QUUX=quux2",
			"--env", "CORGE", // OS exported
			"--env", "GRAULT=grault_key=grault_value", // value contains `=` char
			"--env", "GARPLY=", // OS exported
			"--env", "WALDO=", // not exported in OS
			testutil.CommonImage, "env")
	}

	validate := []test.Comparator{
		expect.Contains(
			"\nFOO=foo1,foo2\n",
			"\nBAR=bar1 bar2\n",
			"\nQUUX=quux2\n",
			"\nCORGE=corge-value-in-host\n",
			"\nGRAULT=grault_key=grault_value\n",
		),
		expect.DoesNotContain("QUX"),
	}

	if runtime.GOOS != "windows" {
		validate = append(
			validate,
			expect.Contains(
				"\nBAZ=\n",
				"\nGARPLY=\n",
				"\nWALDO=\n",
			),
		)
	}

	testCase.Expected = test.Expects(expect.ExitCodeSuccess, nil, expect.All(validate...))

	testCase.Run(t)
}

func TestRunHostnameEnv(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.SubTests = []*test.Case{
		{
			Description: "default hostname",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				cmd := helpers.Command("run", "--rm", "--quiet", testutil.CommonImage)
				// Note: on Windows, just straight passing the command will not work (some cmd escaping weirdness?)
				cmd.Feed(strings.NewReader(`[[ "HOSTNAME=$(hostname)" == "$(env | grep HOSTNAME)" ]]`))
				return cmd
			},
			Expected: test.Expects(expect.ExitCodeSuccess, nil, nil),
		},
		{
			Description: "with --hostname",
			// Windows does not support --hostname
			Require:  require.Not(require.Windows),
			Command:  test.Command("run", "--rm", "--quiet", "--hostname", "foobar", testutil.CommonImage, "env"),
			Expected: test.Expects(expect.ExitCodeSuccess, nil, expect.Contains("HOSTNAME=foobar")),
		},
	}

	testCase.Run(t)
}

func TestRunStdin(t *testing.T) {
	testCase := nerdtest.Setup()

	const testStr = "test-run-stdin"

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		cmd := helpers.Command("run", "--rm", "-i", testutil.CommonImage, "cat")
		cmd.Feed(strings.NewReader(testStr))
		return cmd
	}

	testCase.Expected = test.Expects(expect.ExitCodeSuccess, nil, expect.Equals(testStr))

	testCase.Run(t)
}

func TestRunWithJsonFileLogDriver(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "-d", "--log-driver", "json-file", "--log-opt", "max-size=5K", "--log-opt", "max-file=2", "--name", data.Identifier(), testutil.CommonImage,
			"sh", "-euxc", "hexdump -C /dev/urandom | head -n1000")
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rm", "-f", data.Identifier())
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		time.Sleep(3 * time.Second)
		return helpers.Command("inspect", data.Identifier())
	}

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(func(stdout string, tt tig.T) {
				inspectedContainer := nerdtest.InspectContainer(helpers, data.Identifier())
				logJSONPath := filepath.Dir(inspectedContainer.LogPath)
				// matches = current log file + old log files to retain
				matches, err := filepath.Glob(filepath.Join(logJSONPath, inspectedContainer.ID+"*"))
				assert.NilError(tt, err)
				assert.Assert(tt, len(matches) == 2, "the number of log files is not equal to 2 files, got: %s", matches)
				for _, file := range matches {
					fInfo, err := os.Stat(file)
					assert.NilError(tt, err)
					// The log file size is compared to 5200 bytes (instead 5k) to keep docker compatibility.
					// Docker log rotation lacks precision because the size check is done at the log entry level
					// and not at the byte level (io.Writer), so docker log files can exceed 5k
					assert.Assert(tt, fInfo.Size() <= 5200, "file size exceeded 5k")
				}
			}),
		}
	}

	testCase.Run(t)
}

func TestRunWithJsonFileLogDriverAndLogPathOpt(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.All(
		require.Not(require.Windows),
		require.Not(nerdtest.Docker),
	)

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		customLogJSONPath := filepath.Join(data.Temp().Path(), data.Identifier(), data.Identifier()+"-json.log")
		data.Labels().Set("customLogJSONPath", customLogJSONPath)
		helpers.Ensure("run", "-d", "--log-driver", "json-file", "--log-opt", fmt.Sprintf("log-path=%s", customLogJSONPath), "--log-opt", "max-size=5K", "--log-opt", "max-file=2", "--name", data.Identifier(), testutil.CommonImage,
			"sh", "-euxc", "hexdump -C /dev/urandom | head -n1000")
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rm", "-f", data.Identifier())
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		time.Sleep(3 * time.Second)
		return helpers.Command("inspect", data.Identifier())
	}

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(func(stdout string, tt tig.T) {
				customLogJSONPath := data.Labels().Get("customLogJSONPath")
				rawBytes, err := os.ReadFile(customLogJSONPath)
				assert.NilError(tt, err)
				assert.Assert(tt, len(rawBytes) > 0, "logs are not written correctly to log-path: %s", customLogJSONPath)

				// matches = current log file + old log files to retain
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(customLogJSONPath), data.Identifier()+"*"))
				assert.NilError(tt, err)
				assert.Assert(tt, len(matches) == 2, "the number of log files is not equal to 2 files, got: %s", matches)
				for _, file := range matches {
					fInfo, err := os.Stat(file)
					assert.NilError(tt, err)
					assert.Assert(tt, fInfo.Size() <= 5200, "file size exceeded 5k")
				}
			}),
		}
	}

	testCase.Run(t)
}

func TestRunWithJournaldLogDriver(t *testing.T) {
	testutil.RequireExecutable(t, "journalctl")
	journalctl, _ := exec.LookPath("journalctl")
	res := icmd.RunCmd(icmd.Command(journalctl, "-xe"))
	if res.ExitCode != 0 {
		t.Skipf("current user is not allowed to access journal logs: %s", res.Combined())
	}

	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "-d", "--log-driver", "journald", "--name", data.Identifier(), testutil.CommonImage,
			"sh", "-euxc", "echo foo; echo bar")
		time.Sleep(3 * time.Second)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rm", "-f", data.Identifier())
	}

	testCase.SubTests = []*test.Case{
		{
			Description: "filter journald logs using SYSLOG_IDENTIFIER field",
			Command:     test.Command("version"),
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					ExitCode: expect.ExitCodeSuccess,
					Output: expect.All(func(stdout string, tt tig.T) {
						inspectedContainer := nerdtest.InspectContainer(helpers, data.Identifier())
						filter := fmt.Sprintf("SYSLOG_IDENTIFIER=%s", inspectedContainer.ID[:12])
						found := 0
						check := func(log poll.LogT) poll.Result {
							res := icmd.RunCmd(icmd.Command(journalctl, "--no-pager", "--since", "2 minutes ago", filter))
							if res.ExitCode != 0 {
								return poll.Continue("journalctl command failed")
							}
							if strings.Contains(res.Stdout(), "bar") && strings.Contains(res.Stdout(), "foo") {
								found = 1
								return poll.Success()
							}
							return poll.Continue("reading from journald is not yet finished")
						}
						poll.WaitOn(t, check, poll.WithDelay(100*time.Microsecond), poll.WithTimeout(20*time.Second))
						assert.Equal(tt, 1, found)
					}),
				}
			},
		},
		{
			Description: "filter journald logs using CONTAINER_NAME field",
			Command:     test.Command("version"),
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					ExitCode: expect.ExitCodeSuccess,
					Output: expect.All(func(stdout string, tt tig.T) {
						filter := fmt.Sprintf("CONTAINER_NAME=%s", data.Identifier())
						found := 0
						check := func(log poll.LogT) poll.Result {
							res := icmd.RunCmd(icmd.Command(journalctl, "--no-pager", "--since", "2 minutes ago", filter))
							if res.ExitCode != 0 {
								return poll.Continue("journalctl command failed")
							}
							if strings.Contains(res.Stdout(), "bar") && strings.Contains(res.Stdout(), "foo") {
								found = 1
								return poll.Success()
							}
							return poll.Continue("reading from journald is not yet finished")
						}
						poll.WaitOn(t, check, poll.WithDelay(100*time.Microsecond), poll.WithTimeout(20*time.Second))
						assert.Equal(tt, 1, found)
					}),
				}
			},
		},
		{
			Description: "filter journald logs using IMAGE_NAME field",
			Command:     test.Command("version"),
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					ExitCode: expect.ExitCodeSuccess,
					Output: expect.All(func(stdout string, tt tig.T) {
						filter := fmt.Sprintf("IMAGE_NAME=%s", testutil.CommonImage)
						found := 0
						check := func(log poll.LogT) poll.Result {
							res := icmd.RunCmd(icmd.Command(journalctl, "--no-pager", "--since", "2 minutes ago", filter))
							if res.ExitCode != 0 {
								return poll.Continue("journalctl command failed")
							}
							if strings.Contains(res.Stdout(), "bar") && strings.Contains(res.Stdout(), "foo") {
								found = 1
								return poll.Success()
							}
							return poll.Continue("reading from journald is not yet finished")
						}
						poll.WaitOn(t, check, poll.WithDelay(100*time.Microsecond), poll.WithTimeout(20*time.Second))
						assert.Equal(tt, 1, found)
					}),
				}
			},
		},
	}

	testCase.Run(t)
}

func TestRunWithJournaldLogDriverAndLogOpt(t *testing.T) {
	testutil.RequireExecutable(t, "journalctl")
	journalctl, _ := exec.LookPath("journalctl")
	res := icmd.RunCmd(icmd.Command(journalctl, "-xe"))
	if res.ExitCode != 0 {
		t.Skipf("current user is not allowed to access journal logs: %s", res.Combined())
	}

	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "-d", "--log-driver", "journald", "--log-opt", "tag={{.FullID}}", "--name", data.Identifier(), testutil.CommonImage,
			"sh", "-euxc", "echo foo; echo bar")
		time.Sleep(3 * time.Second)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rm", "-f", data.Identifier())
	}

	testCase.Command = test.Command("version")

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(func(stdout string, tt tig.T) {
				inspectedContainer := nerdtest.InspectContainer(helpers, data.Identifier())
				found := 0
				check := func(log poll.LogT) poll.Result {
					res := icmd.RunCmd(icmd.Command(journalctl, "--no-pager", "--since", "2 minutes ago", fmt.Sprintf("SYSLOG_IDENTIFIER=%s", inspectedContainer.ID)))
					if res.ExitCode != 0 {
						return poll.Continue("journalctl command failed")
					}
					if strings.Contains(res.Stdout(), "bar") && strings.Contains(res.Stdout(), "foo") {
						found = 1
						return poll.Success()
					}
					return poll.Continue("reading from journald is not yet finished")
				}
				poll.WaitOn(t, check, poll.WithDelay(100*time.Microsecond), poll.WithTimeout(20*time.Second))
				assert.Equal(tt, 1, found)
			}),
		}
	}

	testCase.Run(t)
}

func TestRunWithLogBinary(t *testing.T) {
	testutil.RequiresBuild(t)

	testCase := nerdtest.Setup()

	testCase.Require = require.All(
		nerdtest.Build,
		require.Not(require.Windows),
		require.Not(nerdtest.Docker),
	)

	testCase.NoParallel = true

	var dockerfile = `
FROM ` + testutil.GolangImage + ` as builder
WORKDIR /go/src/
RUN mkdir -p logger
WORKDIR /go/src/logger
RUN echo '\
	package main \n\
	\n\
	import ( \n\
	"bufio" \n\
	"context" \n\
	"fmt" \n\
	"io" \n\
	"os" \n\
	"path/filepath" \n\
	"sync" \n\
	\n\
	"github.com/containerd/containerd/v2/core/runtime/v2/logging"\n\
	)\n\

	func main() {\n\
		logging.Run(log)\n\
	}\n\

	func log(ctx context.Context, config *logging.Config, ready func() error) error {\n\
		var wg sync.WaitGroup \n\
		wg.Add(2) \n\
		// forward both stdout and stderr to temp files \n\
		go copy(&wg, config.Stdout, config.ID, "stdout") \n\
		go copy(&wg, config.Stderr, config.ID, "stderr") \n\

		// signal that we are ready and setup for the container to be started \n\
		if err := ready(); err != nil { \n\
		return err \n\
		} \n\
		wg.Wait() \n\
		return nil \n\
	}\n\
	\n\
	func copy(wg *sync.WaitGroup, r io.Reader, id string, kind string) { \n\
		f, _ := os.Create(filepath.Join(os.TempDir(), fmt.Sprintf("%s_%s.log", id, kind))) \n\
		defer f.Close() \n\
		defer wg.Done() \n\
		s := bufio.NewScanner(r) \n\
		for s.Scan() { \n\
			f.WriteString(s.Text()) \n\
		} \n\
	}\n' >> main.go


RUN go mod init
# Workaround for "package slices is not in GOROOT" https://github.com/containerd/nerdctl/issues/4214
RUN go get github.com/containerd/containerd/v2@v2.0.5
RUN go mod tidy
RUN go build .

FROM scratch
COPY --from=builder /go/src/logger/logger /
	`

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		data.Temp().Save(dockerfile, "Dockerfile")
		helpers.Ensure("build", data.Temp().Path(), "--output", fmt.Sprintf("type=local,src=/go/src/logger/logger,dest=%s", data.Temp().Path()))
		helpers.Ensure("run", "-d", "--log-driver", fmt.Sprintf("binary://%s/logger", data.Temp().Path()), "--name", data.Identifier(), testutil.CommonImage,
			"sh", "-euxc", "echo foo; echo bar")
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("container", "rm", "-f", data.Identifier())
		helpers.Anyhow("image", "rm", "-f", data.Identifier())
	}

	testCase.Command = test.Command("version")

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(func(stdout string, t tig.T) {
				inspectedContainer := nerdtest.InspectContainer(helpers, data.Identifier())
				bytes, err := os.ReadFile(filepath.Join(os.TempDir(), fmt.Sprintf("%s_%s.log", inspectedContainer.ID, "stdout")))
				assert.NilError(t, err)
				log := string(bytes)
				assert.Check(t, strings.Contains(log, "foo"))
				assert.Check(t, strings.Contains(log, "bar"))
			}),
		}
	}

	testCase.Run(t)
}

// history: There was a bug that the --add-host items disappear when the another container created.
// This test ensures that it doesn't happen.
// (https://github.com/containerd/nerdctl/issues/2560)
func TestRunAddHostRemainsWhenAnotherContainerCreated(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	hostMapping := "test-add-host:10.0.0.1"

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("run", "-d", "--add-host", hostMapping, "--name", data.Identifier(), testutil.CommonImage, "sleep", nerdtest.Infinity)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("container", "rm", "-f", data.Identifier())
	}

	checkEtcHosts := func(stdout string) error {
		matcher, err := regexp.Compile(`^10.0.0.1\s+test-add-host$`)
		if err != nil {
			return err
		}
		var found bool
		sc := bufio.NewScanner(bytes.NewBufferString(stdout))
		for sc.Scan() {
			if matcher.Match(sc.Bytes()) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("host not found")
		}
		return nil
	}

	testCase.SubTests = []*test.Case{
		{
			Description: "check /etc/hosts before another container",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				return helpers.Command("exec", data.Identifier(), "cat", "/etc/hosts")
			},
			Expected: test.Expects(expect.ExitCodeSuccess, nil, func(stdout string, t tig.T) {
				assert.NilError(t, checkEtcHosts(stdout))
			}),
		},
		{
			Description: "run another container",
			Command:     test.Command("run", "--rm", testutil.CommonImage),
			Expected:    test.Expects(expect.ExitCodeSuccess, nil, nil),
		},
		{
			Description: "check /etc/hosts after another container",
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				return helpers.Command("exec", data.Identifier(), "cat", "/etc/hosts")
			},
			Expected: test.Expects(expect.ExitCodeSuccess, nil, func(stdout string, t tig.T) {
				assert.NilError(t, checkEtcHosts(stdout))
			}),
		},
	}

	testCase.Run(t)
}

func TestRunRmTime(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Ensure("pull", "--quiet", testutil.CommonImage)
		data.Labels().Set("t0", fmt.Sprintf("%d", time.Now().UnixNano()))
	}

	testCase.Command = test.Command("run", "--rm", testutil.CommonImage, "true")

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(func(stdout string, tt tig.T) {
				t0Nano, _ := strconv.ParseInt(data.Labels().Get("t0"), 10, 64)
				t0 := time.Unix(0, t0Nano)
				t1 := time.Now()
				took := t1.Sub(t0)
				var deadline = 3 * time.Second
				// FIXME: Investigate? it appears that since the move to containerd 2 on Windows, this is taking longer.
				if runtime.GOOS == "windows" {
					deadline = 10 * time.Second
				}
				assert.Assert(tt, took <= deadline, "expected to have completed in %v, took %v", deadline, took)
			}),
		}
	}

	testCase.Run(t)
}

func TestRunAttachFlag(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	type subTestCase struct {
		name        string
		args        []string
		withStdin   bool
		stdinInput  string
		testStr     string
		expectedOut string
		dockerOut   string
	}

	testCases := []subTestCase{
		{
			name:        "AttachFlagStdin",
			args:        []string{"-a", "STDIN", "-a", "STDOUT"},
			withStdin:   true,
			stdinInput:  "echo test-run-stdio\nexit\n",
			testStr:     "test-run-stdio",
			expectedOut: "test-run-stdio",
			dockerOut:   "test-run-stdio",
		},
		{
			name:        "AttachFlagStdOut",
			args:        []string{"-a", "STDOUT"},
			withStdin:   false,
			testStr:     "foo",
			expectedOut: "foo",
			dockerOut:   "foo",
		},
		{
			name:        "AttachFlagMixedValue",
			args:        []string{"-a", "STDIN", "-a", "invalid-value"},
			withStdin:   false,
			testStr:     "foo",
			expectedOut: "invalid stream specified with -a flag. Valid streams are STDIN, STDOUT, and STDERR",
			dockerOut:   "valid streams are STDIN, STDOUT and STDERR",
		},
		{
			name:        "AttachFlagInvalidValue",
			args:        []string{"-a", "invalid-stream"},
			withStdin:   false,
			testStr:     "foo",
			expectedOut: "invalid stream specified with -a flag. Valid streams are STDIN, STDOUT, and STDERR",
			dockerOut:   "valid streams are STDIN, STDOUT and STDERR",
		},
		{
			name:        "AttachFlagCaseInsensitive",
			args:        []string{"-a", "stdin", "-a", "stdout"},
			withStdin:   true,
			stdinInput:  "echo test-run-stdio\nexit\n",
			testStr:     "test-run-stdio",
			expectedOut: "test-run-stdio",
			dockerOut:   "test-run-stdio",
		},
	}

	for _, tc := range testCases {
		tc := tc
		testCase.SubTests = append(testCase.SubTests, &test.Case{
			Description: tc.name,
			Cleanup: func(data test.Data, helpers test.Helpers) {
				helpers.Anyhow("rm", "-f", data.Identifier())
			},
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				var fullArgs []string
				if tc.withStdin {
					fullArgs = []string{"run", "--rm", "-i"}
					fullArgs = append(fullArgs, tc.args...)
					fullArgs = append(fullArgs, "--name", data.Identifier(), testutil.CommonImage)
					cmd := helpers.Command(fullArgs...)
					cmd.Feed(strings.NewReader(tc.stdinInput))
					return cmd
				} else {
					fullArgs = []string{"run"}
					fullArgs = append(fullArgs, tc.args...)
					fullArgs = append(fullArgs, "--name", data.Identifier(), testutil.CommonImage, "sh", "-euxc", "echo "+tc.testStr)
					return helpers.Command(fullArgs...)
				}
			},
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				return &test.Expected{
					Output: func(stdout string, t tig.T) {
						errorMsg := fmt.Sprintf("%s failed;\nExpected: '%s'\nActual: '%s'", tc.name, tc.expectedOut, stdout)
						if nerdtest.IsDocker() {
							assert.Equal(t, true, strings.Contains(stdout, tc.dockerOut), errorMsg)
						} else {
							assert.Equal(t, true, strings.Contains(stdout, tc.expectedOut), errorMsg)
						}
					},
				}
			},
		})
	}

	testCase.Run(t)
}

func TestRunQuiet(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rmi", "-f", testutil.CommonImage)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rmi", "-f", testutil.CommonImage)
	}

	sentinel := "test run quiet"
	testCase.Command = test.Command("run", "--rm", "--quiet", testutil.CommonImage, fmt.Sprintf(`echo "%s"`, sentinel))

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(
				expect.Contains(sentinel),
				func(stdout string, t tig.T) {
					wasQuiet := func(output, sentinel string) bool {
						return !strings.Contains(output, sentinel)
					}

					// Docker and nerdctl image pulls are not 1:1.
					var pullSentinel string
					if nerdtest.IsDocker() {
						pullSentinel = "Pull complete"
					} else {
						pullSentinel = "resolved"
					}

					assert.Assert(t, wasQuiet(stdout, pullSentinel), "Found %s in container run output", pullSentinel)
				},
			),
		}
	}

	testCase.Run(t)
}

func TestRunFromOCIArchive(t *testing.T) {
	testutil.RequiresBuild(t)
	testutil.RegisterBuildCacheCleanup(t)

	testCase := nerdtest.Setup()

	// Docker does not support running container images from OCI archive.
	testCase.Require = require.All(
		nerdtest.Build,
		require.Not(nerdtest.Docker),
	)

	const sentinel = "test-nerdctl-run-from-oci-archive"

	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rmi", "-f", data.Identifier())
		dockerfile := fmt.Sprintf(`FROM %s
	CMD ["echo", "%s"]`, testutil.CommonImage, sentinel)
		data.Temp().Save(dockerfile, "Dockerfile")
		tag := fmt.Sprintf("%s:latest", data.Identifier())
		tarPath := filepath.Join(data.Temp().Path(), data.Identifier()+".tar")
		helpers.Ensure("build", "--tag", tag, fmt.Sprintf("--output=type=oci,dest=%s", tarPath), data.Temp().Path())
		data.Labels().Set("tarPath", tarPath)
		data.Labels().Set("tag", tag)
	}

	testCase.Cleanup = func(data test.Data, helpers test.Helpers) {
		helpers.Anyhow("rmi", "-f", data.Identifier())
	}

	testCase.Command = func(data test.Data, helpers test.Helpers) test.TestableCommand {
		return helpers.Command("run", "--rm", fmt.Sprintf("oci-archive://%s", data.Labels().Get("tarPath")))
	}

	testCase.Expected = func(data test.Data, helpers test.Helpers) *test.Expected {
		return &test.Expected{
			ExitCode: expect.ExitCodeSuccess,
			Output: expect.All(
				expect.Contains(fmt.Sprintf("Loaded image: %s", data.Labels().Get("tag"))),
				expect.Contains(sentinel),
			),
		}
	}

	testCase.Run(t)
}

func TestRunDomainname(t *testing.T) {
	testCase := nerdtest.Setup()

	testCase.Require = require.Not(require.Windows)

	testCases := []struct {
		name        string
		hostname    string
		domainname  string
		Cmd         string
		CmdFlag     string
		expectedOut string
	}{
		{
			name:        "Check domain name",
			hostname:    "foobar",
			domainname:  "example.com",
			Cmd:         "hostname",
			CmdFlag:     "-d",
			expectedOut: "example.com",
		},
		{
			name:        "check fqdn",
			hostname:    "foobar",
			domainname:  "example.com",
			Cmd:         "hostname",
			CmdFlag:     "-f",
			expectedOut: "foobar.example.com",
		},
	}

	for _, tc := range testCases {
		tc := tc
		testCase.SubTests = append(testCase.SubTests, &test.Case{
			Description: tc.name,
			Command: test.Command("run",
				"--rm",
				"--hostname", tc.hostname,
				"--domainname", tc.domainname,
				testutil.CommonImage,
				tc.Cmd,
				tc.CmdFlag,
			),
			Expected: test.Expects(expect.ExitCodeSuccess, nil, expect.Contains(tc.expectedOut)),
		})
	}

	testCase.Run(t)
}

func TestRunHealthcheckFlags(t *testing.T) {
	if rootlessutil.IsRootless() {
		t.Skip("healthcheck tests are skipped in rootless environment")
	}
	testCase := nerdtest.Setup()

	testCases := []struct {
		name              string
		args              []string
		shouldFail        bool
		expectTest        []string
		expectRetries     int
		expectInterval    time.Duration
		expectTimeout     time.Duration
		expectStartPeriod time.Duration
	}{
		{
			name: "Valid_full_config",
			args: []string{
				"--health-cmd", "curl -f http://localhost || exit 1",
				"--health-interval", "30s",
				"--health-timeout", "5s",
				"--health-retries", "3",
				"--health-start-period", "2s",
			},
			expectTest:        []string{"CMD-SHELL", "curl -f http://localhost || exit 1"},
			expectInterval:    30 * time.Second,
			expectTimeout:     5 * time.Second,
			expectRetries:     3,
			expectStartPeriod: 2 * time.Second,
		},
		{
			name: "No_healthcheck",
			args: []string{
				"--no-healthcheck",
			},
			expectTest: []string{"NONE"},
		},
		{
			name:       "No_healthcheck_flag",
			args:       []string{},
			expectTest: nil,
		},
		{
			name: "Conflicting_flags",
			args: []string{
				"--no-healthcheck", "--health-cmd", "true",
			},
			shouldFail: true,
		},
		{
			name: "Negative_retries",
			args: []string{
				"--health-cmd", "true",
				"--health-retries", "-2",
			},
			shouldFail: true,
		},
		{
			name: "Negative_timeout",
			args: []string{
				"--health-cmd", "true",
				"--health-timeout", "-5s",
			},
			shouldFail: true,
		},
		{
			name: "Invalid_timeout_format",
			args: []string{
				"--health-cmd", "true",
				"--health-timeout", "5blah",
			},
			shouldFail: true,
		},
		{
			name: "Health_cmd_cmd_shell",
			args: []string{
				"--health-cmd", "curl -f http://localhost || exit 1",
			},
			expectTest: []string{"CMD-SHELL", "curl -f http://localhost || exit 1"},
		},
		{
			name: "Health_cmd_array_like",
			args: []string{
				"--health-cmd", "echo hello",
			},
			expectTest: []string{"CMD-SHELL", "echo hello"},
		},
		{
			name: "Health_cmd_empty",
			args: []string{
				"--health-cmd", "",
				"--health-retries", "2",
			},
			expectTest:    nil,
			expectRetries: 2,
		},
	}

	for _, tc := range testCases {
		tc := tc

		testCase.SubTests = append(testCase.SubTests, &test.Case{
			Description: tc.name,
			Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
				args := append([]string{"run", "-d", "--name", tc.name}, tc.args...)
				args = append(args, testutil.CommonImage, "sleep", "infinity")
				return helpers.Command(args...)
			},
			Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
				if tc.shouldFail {
					return &test.Expected{
						ExitCode: expect.ExitCodeGenericFail,
					}
				}
				return &test.Expected{
					ExitCode: expect.ExitCodeSuccess,
					Output: expect.All(
						func(stdout string, t tig.T) {
							inspect := nerdtest.InspectContainer(helpers, tc.name)
							hc := inspect.Config.Healthcheck
							if tc.expectTest == nil {
								assert.Assert(t, hc == nil || len(hc.Test) == 0)
							} else {
								assert.Assert(t, hc != nil)
								assert.DeepEqual(t, hc.Test, tc.expectTest)
							}
							if tc.expectRetries > 0 {
								assert.Equal(t, hc.Retries, tc.expectRetries)
							}
							if tc.expectTimeout > 0 {
								assert.Equal(t, hc.Timeout, tc.expectTimeout)
							}
							if tc.expectInterval > 0 {
								assert.Equal(t, hc.Interval, tc.expectInterval)
							}
							if tc.expectStartPeriod > 0 {
								assert.Equal(t, hc.StartPeriod, tc.expectStartPeriod)
							}
						},
					),
				}
			},
			Cleanup: func(data test.Data, helpers test.Helpers) {
				helpers.Anyhow("rm", "-f", tc.name)
			},
		})
	}

	testCase.Run(t)
}

func TestRunHealthcheckFromImage(t *testing.T) {
	if rootlessutil.IsRootless() {
		t.Skip("healthcheck tests are skipped in rootless environment")
	}
	nerdtest.Setup()

	dockerfile := fmt.Sprintf(`FROM %s
HEALTHCHECK --interval=30s --timeout=10s CMD wget -q --spider http://localhost:8080 || exit 1
	`, testutil.CommonImage)

	testCase := &test.Case{
		Require: nerdtest.Build,
		Setup: func(data test.Data, helpers test.Helpers) {
			data.Temp().Save(dockerfile, "Dockerfile")
			data.Labels().Set("image", data.Identifier())
			helpers.Ensure("build", "-t", data.Labels().Get("image"), data.Temp().Path())
		},
		SubTests: []*test.Case{
			{
				Description: "merge_with_image",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command("run", "-d", "--name", data.Identifier(),
						"--health-retries=5",
						"--health-interval=45s",
						data.Labels().Get("image"))
				},
				Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
					return &test.Expected{
						ExitCode: expect.ExitCodeSuccess,
						Output: expect.All(func(stdout string, t tig.T) {
							inspect := nerdtest.InspectContainer(helpers, data.Identifier())
							hc := inspect.Config.Healthcheck
							assert.Assert(t, hc != nil, "expected healthcheck config to be present")
							assert.DeepEqual(t, hc.Test, []string{"CMD-SHELL", "wget -q --spider http://localhost:8080 || exit 1"})
							assert.Equal(t, 5, hc.Retries)               // From CLI flags
							assert.Equal(t, 45*time.Second, hc.Interval) // From CLI flags
							assert.Equal(t, 10*time.Second, hc.Timeout)  // From Dockerfile
						}),
					}
				},
				Cleanup: func(data test.Data, helpers test.Helpers) {
					helpers.Anyhow("rm", "-f", data.Identifier())
				},
			},
			{
				Description: "Disable image health checks via runtime flag",
				Command: func(data test.Data, helpers test.Helpers) test.TestableCommand {
					return helpers.Command(
						"run", "-d", "--name", data.Identifier(),
						"--no-healthcheck",
						data.Labels().Get("image"),
					)
				},
				Expected: func(data test.Data, helpers test.Helpers) *test.Expected {
					return &test.Expected{
						ExitCode: expect.ExitCodeSuccess,
						Output: expect.All(func(stdout string, t tig.T) {
							inspect := nerdtest.InspectContainer(helpers, data.Identifier())
							hc := inspect.Config.Healthcheck
							assert.Assert(t, hc != nil, "expected healthcheck config to be present")
							assert.DeepEqual(t, hc.Test, []string{"NONE"})
						}),
					}
				},
				Cleanup: func(data test.Data, helpers test.Helpers) {
					helpers.Anyhow("rm", "-f", data.Identifier())
				},
			},
		},
	}

	testCase.Run(t)
}

func countFIFOFiles(root string) (int, error) {
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeNamedPipe != 0 {
			count++
		}
		return nil
	})
	return count, err
}
func TestCleanupFIFOs(t *testing.T) {
	if rootlessutil.IsRootless() {
		t.Skip("/run/containerd/fifo/ doesn't exist on rootless")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test is not compatible with windows")
	}
	testutil.DockerIncompatible(t)
	testCase := nerdtest.Setup()
	testCase.NoParallel = true
	testCase.Setup = func(data test.Data, helpers test.Helpers) {
		cmd := helpers.Command("run", "-it", "--rm", testutil.CommonImage, "date")
		cmd.WithPseudoTTY()
		cmd.Run(&test.Expected{
			ExitCode: 0,
		})
		oldNumFifos, err := countFIFOFiles("/run/containerd/fifo/")
		assert.NilError(t, err)

		cmd = helpers.Command("run", "-it", "--rm", testutil.CommonImage, "date")
		cmd.WithPseudoTTY()
		cmd.Run(&test.Expected{
			ExitCode: 0,
		})
		newNumFifos, err := countFIFOFiles("/run/containerd/fifo/")
		assert.NilError(t, err)
		assert.Equal(t, oldNumFifos, newNumFifos)
	}
	testCase.Run(t)
}
