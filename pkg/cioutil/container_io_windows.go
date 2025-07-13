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

package cioutil

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/pkg/errors"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/log"
)

// copyIO is from https://github.com/containerd/containerd/blob/148d21b1ae0718b75718a09ecb307bb874270f59/cio/io_windows.go#L44
func copyIO(_ *exec.Cmd, fifos *cio.FIFOSet, ioset *cio.Streams) (_ *ncio, retErr error) {
	ncios := &ncio{cmd: nil, config: fifos.Config}

	defer func() {
		if retErr != nil {
			_ = ncios.Close()
		}
	}()

	if fifos.Stdin != "" {
		l, err := winio.ListenPipe(fifos.Stdin, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create stdin pipe %s: %w", fifos.Stdin, err)
		}
		ncios.closers = append(ncios.closers, l)

		go func() {
			c, err := l.Accept()
			if err != nil {
				log.L.WithError(err).Errorf("failed to accept stdin connection on %s", fifos.Stdin)
				return
			}

			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)

			io.CopyBuffer(c, ioset.Stdin, *p)
			c.Close()
			l.Close()
		}()
	}

	if fifos.Stdout != "" {
		l, err := winio.ListenPipe(fifos.Stdout, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create stdout pipe %s: %w", fifos.Stdout, err)
		}
		ncios.closers = append(ncios.closers, l)

		go func() {
			c, err := l.Accept()
			if err != nil {
				log.L.WithError(err).Errorf("failed to accept stdout connection on %s", fifos.Stdout)
				return
			}

			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)

			io.CopyBuffer(ioset.Stdout, c, *p)
			c.Close()
			l.Close()
		}()
	}

	if fifos.Stderr != "" {
		l, err := winio.ListenPipe(fifos.Stderr, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create stderr pipe %s: %w", fifos.Stderr, err)
		}
		ncios.closers = append(ncios.closers, l)

		go func() {
			c, err := l.Accept()
			if err != nil {
				log.L.WithError(err).Errorf("failed to accept stderr connection on %s", fifos.Stderr)
				return
			}

			p := bufPool.Get().(*[]byte)
			defer bufPool.Put(p)

			io.CopyBuffer(ioset.Stderr, c, *p)
			c.Close()
			l.Close()
		}()
	}

	return ncios, nil
}

type stdioPipes struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

type delayedConnection struct {
	l    net.Listener
	con  net.Conn
	wg   sync.WaitGroup
	once sync.Once
}

func (dc *delayedConnection) Write(p []byte) (int, error) {
	dc.wg.Wait()
	if dc.con != nil {
		return dc.con.Write(p)
	}
	return 0, errors.New("use of closed network connection")
}

func (dc *delayedConnection) Read(p []byte) (int, error) {
	dc.wg.Wait()
	if dc.con != nil {
		return dc.con.Read(p)
	}
	return 0, errors.New("use of closed network connection")
}

func (dc *delayedConnection) unblockConnectionWaiters() {
	defer dc.once.Do(func() {
		dc.wg.Done()
	})
}

func (dc *delayedConnection) Close() error {
	_ = dc.l.Close()
	if dc.con != nil {
		return dc.con.Close()
	}
	dc.unblockConnectionWaiters()
	return nil
}

func NewDirectIO(ctx context.Context, fifos *cio.FIFOSet) (*cio.DirectIO, error) {
	pipes, err := newStdioPipes(fifos)
	if err != nil {
		return nil, err
	}
	return cio.NewDirectIOFromFIFOSet(ctx, pipes.stdin, pipes.stdout, pipes.stderr, fifos), nil
}

// newStdioPipes creates actual fifos for stdio.
func newStdioPipes(fifos *cio.FIFOSet) (_ *stdioPipes, retErr error) {
	p := &stdioPipes{}
	if fifos.Stdin != "" {
		l, err := winio.ListenPipe(fifos.Stdin, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create stdin pipe %s", fifos.Stdin)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if retErr != nil {
				_ = dc.Close()
			}
		}()
		p.stdin = dc

		go func() {
			conn, err := l.Accept()
			if err != nil {
				_ = dc.Close()
				return
			}
			dc.con = conn
			dc.unblockConnectionWaiters()
		}()
	}

	if fifos.Stdout != "" {
		l, err := winio.ListenPipe(fifos.Stdout, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create stdout pipe %s", fifos.Stdout)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if retErr != nil {
				_ = dc.Close()
			}
		}()
		p.stdout = dc

		go func() {
			conn, err := l.Accept()
			if err != nil {
				_ = dc.Close()
				return
			}
			dc.con = conn
			dc.unblockConnectionWaiters()
		}()
	}

	if fifos.Stderr != "" {
		l, err := winio.ListenPipe(fifos.Stderr, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create stderr pipe %s", fifos.Stderr)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if retErr != nil {
				_ = dc.Close()
			}
		}()
		p.stderr = dc

		go func() {
			conn, err := l.Accept()
			if err != nil {
				_ = dc.Close()
				return
			}
			dc.con = conn
			dc.unblockConnectionWaiters()
		}()
	}
	return p, nil
}
