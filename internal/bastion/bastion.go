// Package bastion reaches a management cluster's API through an SSH jump host
// (PET-102), the way private EKS or GKE clusters are reached: hydra starts the
// system `ssh` with a local port forward to the API server's real address,
// and talks to the API through it.
//
// The system ssh is used, not an SSH library, so everything in ~/.ssh/config
// applies: keys and agents, known_hosts, ProxyJump, and a ProxyCommand that
// goes through Tailscale, Cloudflare Access, AWS SSM or GCP IAP. hydra itself
// knows nothing of how the jump host is reached.
//
// TLS is still verified against the API server's own name: the kubeconfig is
// pointed at the tunnel with tls-server-name set to the original host.
package bastion

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// sshBinary is the ssh client; tests replace it.
var sshBinary = "ssh"

// Tunnel is a running `ssh -N -L` to one API server.
type Tunnel struct {
	// Dest is the jump host as given (user@host[:port]).
	Dest string
	// Target is the API server's host:port the tunnel forwards to.
	Target string
	// Local is the 127.0.0.1:port the tunnel listens on.
	Local string

	cmd     *exec.Cmd
	stderr  *lockedBuffer
	done    chan struct{}
	err     error
	once    sync.Once
	closing atomic.Bool
}

// SplitDest splits user@host[:port] into the ssh destination and its port
// ("" when none is given). An IPv6 host is written [addr] or [addr]:port.
func SplitDest(dest string) (host, port string, err error) {
	dest = strings.TrimSpace(dest)
	if dest == "" || strings.HasPrefix(dest, "-") || strings.ContainsAny(dest, " \t\n") {
		return "", "", fmt.Errorf("--bastion %q: want user@host or user@host:port", dest)
	}
	bad := fmt.Errorf("--bastion %q: want user@host or user@host:port (an IPv6 host as [addr] or [addr]:port)", dest)
	user, rest := "", dest
	if i := strings.LastIndex(dest, "@"); i == 0 {
		return "", "", bad // "@host": an empty user
	} else if i > 0 {
		user, rest = dest[:i+1], dest[i+1:]
	}
	if rest == "" || strings.HasPrefix(rest, ":") || strings.Contains(rest, "/") {
		return "", "", bad
	}
	if strings.HasPrefix(rest, "[") {
		h, p, err := net.SplitHostPort(rest)
		if err != nil {
			if strings.HasSuffix(rest, "]") && len(rest) > 2 {
				return user + strings.Trim(rest, "[]"), "", nil
			}
			return "", "", bad
		}
		if err := validPort(dest, p); err != nil {
			return "", "", err
		}
		return user + h, p, nil
	}
	if strings.Count(rest, ":") > 1 {
		return "", "", bad // an unbracketed IPv6 address: which colon is the port's?
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		if err := validPort(dest, rest[i+1:]); err != nil {
			return "", "", err
		}
		return user + rest[:i], rest[i+1:], nil
	}
	return dest, "", nil
}

func validPort(dest, p string) error {
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("--bastion %q: %q is not a port", dest, p)
	}
	return nil
}

// Open starts a tunnel through dest to the API server of kubeconfig's current
// context and returns it with the kubeconfig pointed at it. The caller must
// Close the tunnel. Open waits up to timeout for the forward to listen.
func Open(ctx context.Context, dest string, kubeconfig []byte, timeout time.Duration) (*Tunnel, []byte, error) {
	sshHost, sshPort, err := SplitDest(dest)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the management kubeconfig: %w", err)
	}
	cluster, err := currentCluster(cfg)
	if err != nil {
		return nil, nil, err
	}
	server, err := url.Parse(cfg.Clusters[cluster].Server)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" {
		return nil, nil, fmt.Errorf("the management kubeconfig's server %q is not an https URL", cfg.Clusters[cluster].Server)
	}
	if c := cfg.Clusters[cluster]; c.InsecureSkipTLSVerify {
		return nil, nil, fmt.Errorf("the management kubeconfig skips TLS verification (insecure-skip-tls-verify); --bastion needs it verified, so that only the real API server gets your credentials through the tunnel")
	} else if c.ProxyURL != "" {
		return nil, nil, fmt.Errorf("the management kubeconfig sets proxy-url %q; --bastion replaces the route to the API server, so use one or the other", c.ProxyURL)
	}
	targetPort := server.Port()
	if targetPort == "" {
		targetPort = "443"
	}
	target := net.JoinHostPort(server.Hostname(), targetPort)

	sshOpts := []string{
		"-o", "ExitOnForwardFailure=yes",
		"-o", "BatchMode=yes", // never prompt: a CI job, or a caller with no agent, fails fast
		// Never share a connection: a ControlMaster would own the forward
		// and keep it open after hydra stops this ssh.
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		// A tunnel needs no agent on the bastion (ProxyJump authenticates
		// each hop with the local agent).
		"-o", "ForwardAgent=no",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if err := refuseConfigForwards(ctx, dest, sshHost, sshPort, sshOpts); err != nil {
		return nil, nil, err
	}

	var lastErr error
	for range 3 { // a freed port can be taken before ssh binds it
		local, err := freePort()
		if err != nil {
			return nil, nil, err
		}
		t := &Tunnel{Dest: dest, Target: target, Local: local, stderr: &lockedBuffer{}, done: make(chan struct{})}
		args := append(append([]string{"-N"}, sshOpts...), "-L", local+":"+target)
		if sshPort != "" {
			args = append(args, "-p", sshPort)
		}
		args = append(args, "--", sshHost)
		t.cmd = exec.CommandContext(ctx, sshBinary, args...)
		t.cmd.Stdin = nil
		t.cmd.Stdout = nil
		t.cmd.Stderr = t.stderr
		// A ProxyCommand's own children can hold stderr open after ssh exits;
		// do not wait for them.
		t.cmd.WaitDelay = 2 * time.Second
		setParentDeath(t.cmd)
		if err := t.cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("starting ssh to bastion %s: %w", dest, err)
		}
		go func() { t.err = t.cmd.Wait(); close(t.done) }()
		if err := t.waitListening(timeout); err != nil {
			_ = t.Close()
			lastErr = err
			if strings.Contains(t.stderr.String(), "Address already in use") {
				continue
			}
			return nil, nil, err
		}
		rewritten, err := pointAt(cfg, cluster, local, server)
		if err != nil {
			_ = t.Close()
			return nil, nil, err
		}
		return t, rewritten, nil
	}
	return nil, nil, lastErr
}

// refuseConfigForwards refuses a bastion whose ssh configuration opens
// forwards of its own (LocalForward, RemoteForward, DynamicForward). They would
// open beside hydra's for as long as the command runs, and a RemoteForward lets
// the bastion reach back into this machine. ssh's ClearAllForwardings would
// also clear hydra's own -L, so the configuration is checked instead.
func refuseConfigForwards(ctx context.Context, dest, host, port string, opts []string) error {
	args := append([]string{"-G"}, opts...)
	if port != "" {
		args = append(args, "-p", port)
	}
	args = append(args, "--", host)
	cmd := exec.CommandContext(ctx, sshBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("reading the ssh configuration for bastion %s (ssh -G): %v: %s", dest, err, strings.TrimSpace(stderr.String()))
	}
	var forwards []string
	for line := range strings.SplitSeq(string(out), "\n") {
		key, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch strings.ToLower(key) {
		case "localforward", "remoteforward", "dynamicforward":
			forwards = append(forwards, strings.TrimSpace(line))
		}
	}
	if len(forwards) > 0 {
		return fmt.Errorf("the ssh configuration for bastion %s opens forwards of its own (%s); they would stay open beside hydra's tunnel. Use a Host entry without them for --bastion", dest, strings.Join(forwards, "; "))
	}
	return nil
}

// waitListening returns once the local end accepts connections, or the
// reason ssh did not get that far.
func (t *Tunnel) waitListening(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-t.done:
			return fmt.Errorf("ssh to bastion %s exited before forwarding %s: %v%s", t.Dest, t.Target, t.err, t.stderrTail())
		default:
		}
		if c, err := net.DialTimeout("tcp", t.Local, 200*time.Millisecond); err == nil {
			_ = c.Close()
			select {
			case <-t.done: // something else holds the port, not our ssh
				return fmt.Errorf("ssh to bastion %s exited before forwarding %s: %v%s", t.Dest, t.Target, t.err, t.stderrTail())
			default:
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("ssh to bastion %s did not forward %s within %s%s", t.Dest, t.Target, timeout, t.stderrTail())
}

// Done is closed when ssh exits, whether by Close or on its own.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Unexpected reports why ssh exited, once it has exited without Close being
// called; "" while it runs or after Close.
func (t *Tunnel) Unexpected() string {
	select {
	case <-t.done:
	default:
		return ""
	}
	if t.closing.Load() {
		return ""
	}
	return fmt.Sprintf("the ssh tunnel to %s through bastion %s exited: %v%s", t.Target, t.Dest, t.err, t.stderrTail())
}

// Diagnose returns ssh's recent messages, for an error that may have been
// caused by the tunnel (such as the bastion failing to reach the API).
func (t *Tunnel) Diagnose() string {
	if t == nil {
		return ""
	}
	if u := t.Unexpected(); u != "" {
		return u
	}
	if tail := t.stderrTail(); tail != "" {
		return "ssh to bastion " + t.Dest + " said" + tail
	}
	return ""
}

func (t *Tunnel) stderrTail() string {
	s := strings.TrimSpace(t.stderr.String())
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return ": " + strings.Join(lines, "; ")
}

// Close stops ssh and waits for it to exit. It is safe to call more than once.
func (t *Tunnel) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		t.closing.Store(true)
		select {
		case <-t.done:
			return
		default:
		}
		_ = t.cmd.Process.Signal(os.Interrupt)
		select {
		case <-t.done:
		case <-time.After(3 * time.Second):
			_ = t.cmd.Process.Kill()
			<-t.done // bounded by WaitDelay
		}
	})
	return nil
}

func currentCluster(cfg *clientcmdapi.Config) (string, error) {
	ctxName := cfg.CurrentContext
	c, ok := cfg.Contexts[ctxName]
	if !ok || c == nil {
		return "", fmt.Errorf("the management kubeconfig has no current context %q", ctxName)
	}
	if _, ok := cfg.Clusters[c.Cluster]; !ok {
		return "", fmt.Errorf("the management kubeconfig's context %q names no cluster %q", ctxName, c.Cluster)
	}
	return c.Cluster, nil
}

// pointAt makes the kubeconfig talk to local while still verifying the
// server certificate for the server's host. A tls-server-name already set is
// kept, and so is any path on the server URL (a proxied API).
func pointAt(cfg *clientcmdapi.Config, cluster, local string, server *url.URL) ([]byte, error) {
	out := cfg.DeepCopy()
	c := out.Clusters[cluster]
	u := *server
	u.Host = local
	c.Server = u.String()
	if c.TLSServerName == "" {
		c.TLSServerName = server.Hostname()
	}
	return clientcmd.Write(*out)
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("finding a free local port for the bastion tunnel: %w", err)
	}
	addr := l.Addr().String()
	return addr, l.Close()
}

// stderrKeep is how much of ssh's stderr is kept: the end of it, where the
// reason ssh exited is.
const stderrKeep = 64 << 10

// lockedBuffer keeps the last stderrKeep bytes of ssh's stderr while it runs.
// A ProxyCommand may log for the whole session; the line that says why ssh
// exited comes last.
type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - stderrKeep; over > 0 {
		cut := over
		// Start at a line, so the tail never opens with half of one.
		if i := bytes.IndexByte(b.buf[over:], '\n'); i >= 0 && i < len(b.buf)-over-1 {
			cut = over + i + 1
		}
		b.buf = append([]byte(nil), b.buf[cut:]...)
	}
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
