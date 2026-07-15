package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// isolatedPython builds a Python runtime with network isolation resolved from the
// platform, skipping the test when unprivileged namespaces are unavailable (e.g.
// hardened kernels, non-Linux). This drives the F-03 egress guarantee end to end.
func isolatedPython(t *testing.T) *interpretedRuntime {
	t.Helper()
	requirePython(t)
	// Force the netns backend (RUNNER_SANDBOX=off) with network isolation resolved
	// from the platform, so this drives the F-03 egress guarantee specifically —
	// independent of whether nsjail happens to be installed.
	sb, err := sandbox.Configure("off", "auto", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	if !sb.NetworkIsolated() {
		t.Skip("unprivileged network namespaces unavailable on this platform")
	}
	return NewPython(sb, 64*1024, 256, 64, 4_000_000)
}

// TestNetworkIsolation_DeniesEgress verifies that isolated code cannot open a
// socket to any host: the run happens inside an empty network namespace,
// independent of the deploy network.
func TestNetworkIsolation_DeniesEgress(t *testing.T) {
	rt := isolatedPython(t)
	src := "import socket\n" +
		"socket.setdefaulttimeout(3)\n" +
		"try:\n" +
		"    socket.create_connection(('1.1.1.1', 80))\n" +
		"    print('CONNECTED')\n" +
		"except OSError as e:\n" +
		"    print('BLOCKED', e.errno)\n"
	res := rt.Run(context.Background(), runnerapi.RunRequest{SourceCode: src, TimeoutMs: 5000, MemoryMB: 128})
	if strings.Contains(res.Stdout, "CONNECTED") {
		t.Fatalf("network isolation failed: code reached the network (stdout=%q)", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "BLOCKED") {
		t.Fatalf("expected a blocked-connection result, got status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestNetworkIsolation_HasNoRoutes confirms the isolated namespace exposes no
// route off-host. Reading the namespace-aware /proc route table is portable to
// outer CI sandboxes that forbid even the socket syscall used by if_nameindex.
func TestNetworkIsolation_HasNoRoutes(t *testing.T) {
	rt := isolatedPython(t)
	res := rt.Run(context.Background(), runnerapi.RunRequest{
		SourceCode: "from pathlib import Path\n" +
			"rows = Path('/proc/net/route').read_text().splitlines()[1:]\n" +
			"print('ROUTES', len([row for row in rows if row.strip()]))\n",
		TimeoutMs: 5000,
		MemoryMB:  128,
	})
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %q (stderr=%q)", res.Status, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ROUTES 0") {
		t.Fatalf("isolated run must have an empty route table, got %q", res.Stdout)
	}
}
