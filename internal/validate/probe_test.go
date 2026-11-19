package validate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"syscall"
	"testing"

	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// timeoutErr is a net.Error that timed out, as a dial past its deadline is.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// dialErr wraps errno the way net.Dialer reports a failed connect.
func dialErr(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno)}
}

func TestClassify(t *testing.T) {
	other := errors.New("permission denied by policy")
	for name, tc := range map[string]struct {
		err     error
		want    Outcome
		wantErr error
	}{
		"refused":          {dialErr(syscall.ECONNREFUSED), Refused, nil},
		"host unreachable": {dialErr(syscall.EHOSTUNREACH), NoHost, nil},
		"net unreachable":  {dialErr(syscall.ENETUNREACH), NoRoute, nil},
		"timeout":          {&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, NoAnswer, nil},
		"anything else":    {other, 0, other},
		// A net.Error that did not time out is not silence.
		"non-timeout net error": {&net.OpError{Op: "dial", Net: "tcp", Err: other}, 0, other},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := classify(tc.err)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, %v; want the error passed through", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestVantageNames(t *testing.T) {
	h := HostProber()
	if h.Where() != thisMachine || !h.OnThisHost() {
		t.Errorf("host prober: %q %v", h.Where(), h.OnThisHost())
	}
	k := &kindVantage{cluster: "hydra-bootstrap"}
	if k.OnThisHost() {
		t.Error("a kind node is not this host")
	}
	if got, want := k.Where(), "the bootstrap cluster's node (hydra-bootstrap)"; got != want {
		t.Errorf("Where() = %q, want %q", got, want)
	}
}

func TestExitCode(t *testing.T) {
	sh, err := osexec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	exitErr := osexec.Command(sh, "-c", "exit 124").Run()
	if exitErr == nil {
		t.Fatal("want a non-zero exit")
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"exit error":                {exitErr, 124},
		"inside kind's RunError":    {&kindexec.RunError{Command: []string{"timeout"}, Inner: exitErr}, 124},
		"wrapped":                   {fmt.Errorf("probe: %w", exitErr), 124},
		"did not run to an exit":    {errors.New("exec: no such container"), -1},
		"RunError without an exit":  {&kindexec.RunError{Inner: errors.New("docker gone")}, -1},
		"nil (ran and exited zero)": {nil, -1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// A probe cut short by the caller's context reports the cancellation, not an
// outcome read from whatever the dying exec printed.
func TestKindVantageHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	k := &kindVantage{cluster: "k", run: func(context.Context, ...string) (string, int, error) {
		cancel()
		return "bash: connect: Connection refused\n", -1, errors.New("signal: killed")
	}}
	o, err := k.TCP(ctx, "10.0.0.1", 22)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, %v; want context.Canceled", o, err)
	}
}

// The verdict words are what the report prints and scripts grep for.
func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{Pass: "PASS", Warn: "WARN", Fail: lcFAIL, Status(9): lcFAIL} {
		if got := s.String(); got != want {
			t.Errorf("Status(%d) = %q, want %q", int(s), got, want)
		}
	}
}
