package validate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// probeTimeout bounds every connect attempt.
const probeTimeout = 3 * time.Second

// hostVantage probes from this machine.
type hostVantage struct{}

// HostProber probes from this machine.
func HostProber() Prober { return hostVantage{} }

// thisMachine names the host vantage in messages.
const thisMachine = "this machine"

func (hostVantage) Where() string { return thisMachine }

func (hostVantage) OnThisHost() bool { return true }

func (hostVantage) TCP(ctx context.Context, host string, port int) (Outcome, error) {
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		_ = conn.Close()
		return Open, nil
	}
	return classify(err)
}

// classify sorts a dial error into what it says about the address.
func classify(err error) (Outcome, error) {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return Refused, nil
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return Unreachable, nil
	case errors.As(err, &ne) && ne.Timeout():
		return NoAnswer, nil
	}
	return 0, err
}

// kindVantage probes from inside the bootstrap cluster's control-plane node
// container, where Cluster API's controllers run.
type kindVantage struct {
	cluster string
	run     func(ctx context.Context, args ...string) (stderr string, err error)
}

// KindProber returns a prober inside kind cluster name's control-plane node,
// or nil when that cluster does not exist.
func KindProber(name string) (Prober, error) {
	opt, err := kindcluster.DetectNodeProvider()
	if err != nil {
		return nil, err
	}
	p := kindcluster.NewProvider(opt)
	names, err := p.List()
	if err != nil {
		return nil, err
	}
	found := false
	for _, n := range names {
		found = found || n == name
	}
	if !found {
		return nil, nil
	}
	nodes, err := p.ListNodes(name)
	if err != nil {
		return nil, err
	}
	cps, err := nodeutils.ControlPlaneNodes(nodes)
	if err != nil || len(cps) == 0 {
		return nil, fmt.Errorf("kind cluster %s has no control-plane node: %v", name, err)
	}
	node := cps[0]
	return &kindVantage{cluster: name, run: func(_ context.Context, args ...string) (string, error) {
		var stderr bytes.Buffer
		err := node.Command(args[0], args[1:]...).SetStderr(&stderr).Run()
		return stderr.String(), err
	}}, nil
}

func (k *kindVantage) OnThisHost() bool { return false }

func (k *kindVantage) Where() string { return "the bootstrap cluster's node (" + k.cluster + ")" }

// TCP connects with bash's /dev/tcp. The host and port are passed as
// arguments, never spliced into the script.
func (k *kindVantage) TCP(ctx context.Context, host string, port int) (Outcome, error) {
	secs := strconv.Itoa(int(probeTimeout / time.Second))
	stderr, err := k.run(ctx, "timeout", secs, "bash", "-c", `exec 3<>"/dev/tcp/$0/$1"`, host, strconv.Itoa(port))
	if err == nil {
		return Open, nil
	}
	switch s := strings.ToLower(stderr); {
	case strings.Contains(s, "connection refused"):
		return Refused, nil
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return Unreachable, nil
	case strings.TrimSpace(s) == "":
		// timeout(1) killed the attempt: nothing answered.
		return NoAnswer, nil
	default:
		return 0, fmt.Errorf("%s", strings.TrimSpace(stderr))
	}
}
