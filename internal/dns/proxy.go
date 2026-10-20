package dns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chrismaximin/dnszoner/internal/config"
)

// httpClient returns the client for a source's calls: direct, or through its
// proxy. TLS still runs end to end between dnszoner and the provider, so the
// proxy only relays encrypted bytes and never sees the credentials.
func httpClient(proxy string) (*http.Client, error) {
	if proxy == "" {
		return http.DefaultClient, nil
	}
	u, err := config.ParseProxy(proxy)
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "ssh" {
		t.Proxy = nil
		t.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return dialSSH(u, addr)
		}
	} else {
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: t}, nil
}

// dialSSH opens a connection to addr from the SSH host, with the system ssh
// (ssh -W), so ~/.ssh/config, keys, the agent and known_hosts all apply.
// BatchMode makes ssh fail rather than prompt, which would garble the UI.
func dialSSH(u *url.URL, addr string) (net.Conn, error) {
	args := []string{"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes", "-W", addr}
	if u.User != nil {
		args = append(args, "-l", u.User.Username())
	}
	if p := u.Port(); p != "" {
		args = append(args, "-p", p)
	}
	args = append(args, "--", u.Hostname())
	cmd := exec.Command("ssh", args...)
	c := &sshConn{cmd: cmd, host: u.Hostname()}
	cmd.Stderr = &c.stderr
	var err error
	if c.w, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if c.r, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proxy: starting ssh: %w", err)
	}
	// ssh connects in the background: the TLS handshake that follows waits
	// for it, and closes the conn if ctx ends first.
	return c, nil
}

// sshConn is a net.Conn over an ssh -W process's stdin and stdout.
type sshConn struct {
	cmd    *exec.Cmd
	host   string
	r      io.ReadCloser
	w      io.WriteCloser
	stderr syncBuffer

	waitOnce sync.Once
	waitErr  error
}

func (c *sshConn) wait() error {
	c.waitOnce.Do(func() { c.waitErr = c.cmd.Wait() })
	return c.waitErr
}

func (c *sshConn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if errors.Is(err, io.EOF) {
		// ssh exiting early (unreachable host, refused key, unknown host
		// key) shows up as EOF: report what ssh said instead.
		if werr := c.wait(); werr != nil {
			msg := strings.TrimSpace(c.stderr.String())
			if msg == "" {
				msg = werr.Error()
			}
			return n, fmt.Errorf("proxy ssh %s: %s", c.host, msg)
		}
	}
	return n, err
}

func (c *sshConn) Write(b []byte) (int, error) { return c.w.Write(b) }

func (c *sshConn) Close() error {
	c.w.Close()
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	c.wait()
	return nil
}

type pipeAddr string

func (a pipeAddr) Network() string { return "ssh" }
func (a pipeAddr) String() string  { return string(a) }

func (c *sshConn) LocalAddr() net.Addr  { return pipeAddr("local") }
func (c *sshConn) RemoteAddr() net.Addr { return pipeAddr(c.host) }

// Deadlines aren't supported on pipes; requests are bounded by their
// context instead, which closes the conn.
func (c *sshConn) SetDeadline(time.Time) error      { return nil }
func (c *sshConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshConn) SetWriteDeadline(time.Time) error { return nil }

// syncBuffer is a bytes.Buffer safe to write from exec's stderr copier while
// being read.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
