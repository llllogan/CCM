package sshx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loganjanssen/ccm/internal/config"
	"github.com/loganjanssen/ccm/internal/model"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Manager struct {
	cfg       *config.Config
	auth      []ssh.AuthMethod
	hostCheck ssh.HostKeyCallback
	conns     map[string]*targetConn
	mu        sync.Mutex
}

type targetConn struct {
	target   *model.Target
	client   *ssh.Client
	mu       sync.Mutex
	logInUse bool
}

func NewManager(cfg *config.Config) (*Manager, error) {
	auth, err := loadAuthMethods()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:       cfg,
		auth:      auth,
		hostCheck: ssh.InsecureIgnoreHostKey(),
		conns:     map[string]*targetConn{},
	}
	for id, t := range cfg.Targets {
		m.conns[id] = &targetConn{target: t}
	}
	return m, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		c.mu.Lock()
		if c.client != nil {
			_ = c.client.Close()
			c.client = nil
		}
		c.mu.Unlock()
	}
}

func (m *Manager) RunCommand(ctx context.Context, targetID, cmd string, timeout time.Duration) (model.CommandResult, error) {
	tc, err := m.target(targetID)
	if err != nil {
		return model.CommandResult{}, err
	}
	client, err := m.controlClient(tc)
	if err != nil {
		return model.CommandResult{}, err
	}
	res, err := run(ctx, client, cmd, timeout)
	if err == nil || !isRetryableConnErr(err) {
		return res, err
	}
	if rerr := m.resetClient(tc); rerr != nil {
		return model.CommandResult{}, rerr
	}
	client, err = m.controlClient(tc)
	if err != nil {
		return model.CommandResult{}, err
	}
	return run(ctx, client, cmd, timeout)
}

func (m *Manager) WriteFile(ctx context.Context, targetID, remotePath string, content []byte, mode string, timeout time.Duration) error {
	tc, err := m.target(targetID)
	if err != nil {
		return err
	}
	client, err := m.controlClient(tc)
	if err != nil {
		return err
	}
	err = m.writeFileWithClient(ctx, client, remotePath, content, mode, timeout)
	if err == nil || !isRetryableConnErr(err) {
		return err
	}
	if rerr := m.resetClient(tc); rerr != nil {
		return rerr
	}
	client, err = m.controlClient(tc)
	if err != nil {
		return err
	}
	return m.writeFileWithClient(ctx, client, remotePath, content, mode, timeout)
}

func (m *Manager) writeFileWithClient(ctx context.Context, client *ssh.Client, remotePath string, content []byte, mode string, timeout time.Duration) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var stderr bytes.Buffer
	session.Stderr = &stderr

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resCh := make(chan error, 1)
	go func() {
		dir := filepath.Dir(remotePath)
		cmd := fmt.Sprintf("mkdir -p %q && cat > %q && chmod %s %q", dir, remotePath, mode, remotePath)
		resCh <- session.Run(cmd)
	}()

	_, werr := io.Copy(stdin, bytes.NewReader(content))
	_ = stdin.Close()
	if werr != nil {
		return werr
	}

	select {
	case err := <-resCh:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) StreamLogs(ctx context.Context, targetID, cmd string, out io.Writer) error {
	tc, err := m.target(targetID)
	if err != nil {
		return err
	}
	client, release, err := m.logClient(tc)
	if err != nil {
		return err
	}
	defer release()

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return err
	}

	if err := session.Start(cmd); err != nil {
		return err
	}

	type streamEvent struct {
		line []byte
		err  error
		done bool
	}
	evCh := make(chan streamEvent, 128)

	stream := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		// Docker logs can include long JSON lines; raise scanner ceiling from 64K.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := append([]byte(scanner.Text()), '\n')
			select {
			case evCh <- streamEvent{line: line}:
			case <-ctx.Done():
				evCh <- streamEvent{done: true}
				return
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			evCh <- streamEvent{err: err, done: true}
			return
		}
		evCh <- streamEvent{done: true}
	}
	go stream(stdout)
	go stream(stderr)

	done := 0
	for done < 2 {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
			return ctx.Err()
		case ev := <-evCh:
			if len(ev.line) > 0 {
				if _, err := out.Write(ev.line); err != nil {
					_ = session.Signal(ssh.SIGKILL)
					_ = session.Close()
					return err
				}
			}
			if ev.err != nil {
				_ = session.Signal(ssh.SIGKILL)
				_ = session.Close()
				return ev.err
			}
			if ev.done {
				done++
			}
		}
	}

	if err := session.Wait(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (m *Manager) target(id string) (*targetConn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tc, ok := m.conns[id]
	if !ok {
		return nil, fmt.Errorf("unknown target: %s", id)
	}
	return tc, nil
}

func (m *Manager) controlClient(tc *targetConn) (*ssh.Client, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.client != nil {
		return tc.client, nil
	}
	c, err := m.dial(tc.target)
	if err != nil {
		return nil, err
	}
	tc.client = c
	return tc.client, nil
}

func (m *Manager) logClient(tc *targetConn) (*ssh.Client, func(), error) {
	tc.mu.Lock()
	if tc.logInUse {
		tc.mu.Unlock()
		return nil, nil, errors.New("log stream already active for target")
	}
	tc.logInUse = true
	tc.mu.Unlock()

	client, err := m.dial(tc.target)
	if err != nil {
		tc.mu.Lock()
		tc.logInUse = false
		tc.mu.Unlock()
		return nil, nil, err
	}

	release := func() {
		_ = client.Close()
		tc.mu.Lock()
		tc.logInUse = false
		tc.mu.Unlock()
	}
	return client, release, nil
}

func (m *Manager) resetClient(tc *targetConn) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.client != nil {
		_ = tc.client.Close()
		tc.client = nil
	}
	return nil
}

func (m *Manager) dial(t *model.Target) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            m.auth,
		HostKeyCallback: m.hostCheck,
		Timeout:         8 * time.Second,
	}
	addr := net.JoinHostPort(t.Host, fmt.Sprintf("%d", t.Port))
	return ssh.Dial("tcp", addr, cfg)
}

func run(ctx context.Context, client *ssh.Client, cmd string, timeout time.Duration) (model.CommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return model.CommandResult{}, err
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	errCh := make(chan error, 1)
	go func() {
		errCh <- session.Run(cmd)
	}()

	select {
	case err := <-errCh:
		res := model.CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
		if err == nil {
			return res, nil
		}
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		return res, err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return model.CommandResult{}, ctx.Err()
	}
}

func loadAuthMethods() ([]ssh.AuthMethod, error) {
	methods := []ssh.AuthMethod{}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agentSigner(conn)))
		}
	}

	keyPath := os.Getenv("CCM_SSH_KEY")
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, ".ssh", "id_ed25519")
	}
	if b, err := os.ReadFile(keyPath); err == nil {
		signer, err := ssh.ParsePrivateKey(b)
		if err == nil {
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}

	if len(methods) == 0 {
		return nil, errors.New("no SSH auth methods available; provide agent or CCM_SSH_KEY")
	}
	return methods, nil
}

func agentSigner(conn net.Conn) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		c := agent.NewClient(conn)
		return c.Signers()
	}
}

func isRetryableConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection")
}
