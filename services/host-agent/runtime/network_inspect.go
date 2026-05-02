package runtime

// network_inspect.go — Inspection and safe cleanup helpers for privileged
// Linux networking acceptance tests.
//
// These helpers use real ip(8) and iptables(8) commands. They are only called
// from tests guarded by VM_PLATFORM_ENABLE_NET_TESTS=1 — never from production
// code paths or default (FakeExecutor) unit tests.
//
// All cleanup functions use deterministic "cpvm-test-*" identifiers to ensure
// they never touch production rules. No destructive host-wide iptables flushes
// are performed.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// ── TAP inspection ────────────────────────────────────────────────────────────

// TAPExists returns true if a TAP device identified by tapDev currently
// exists in the kernel.
func (n *NetworkManager) TAPExists(ctx context.Context, tapDev string) bool {
	out, err := n.executor.RunOutput(ctx, "ip", "link", "show", tapDev)
	if err != nil {
		return false
	}
	return strings.Contains(out, tapDev)
}

// TAPHasBridge checks whether a TAP device is attached to the given bridge.
func (n *NetworkManager) TAPHasBridge(ctx context.Context, tapDev, bridge string) bool {
	out, err := n.executor.RunOutput(ctx, "ip", "link", "show", tapDev)
	if err != nil {
		return false
	}
	return strings.Contains(out, "master "+bridge)
}

// ── iptables rule inspection ──────────────────────────────────────────────────

// NATRuleExists checks whether an iptables NAT rule described by args exists.
// appendArgs should use "-A" form; this method rewrites to "-C" for checking.
func (n *NetworkManager) NATRuleExists(ctx context.Context, appendArgs []string) bool {
	checkArgs := copyArgsWithReplace(appendArgs, "-A", "-C")
	_, err := n.executor.RunOutput(ctx, "iptables", checkArgs...)
	return err == nil
}

// FilterRuleExists checks whether an iptables filter table rule described by
// args exists.
func (n *NetworkManager) FilterRuleExists(ctx context.Context, appendArgs []string) bool {
	checkArgs := copyArgsWithReplace(appendArgs, "-A", "-C")
	_, err := n.executor.RunOutput(ctx, "iptables", checkArgs...)
	return err == nil
}

// ChainExists returns true if the named iptables chain exists in the filter table.
func (n *NetworkManager) ChainExists(ctx context.Context, chain string) bool {
	out, err := n.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", chain, "-n")
	if err != nil {
		return false
	}
	return !strings.Contains(out, "No chain/target/match by that name")
}

// RuleCountInFilterChain returns the number of rules in a filter table chain
// (excluding the chain header/policy lines).
func (n *NetworkManager) RuleCountInFilterChain(ctx context.Context, chain string) int {
	out, err := n.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", chain, "-n")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Chain") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		count++
	}
	return count
}

// ── Safe cleanup (deterministic instance IDs only) ────────────────────────────

// SafeCleanupTAP removes a TAP device by its exact name. Idempotent.
func (n *NetworkManager) SafeCleanupTAP(ctx context.Context, tapDev string) {
	_, existsErr := n.executor.RunOutput(ctx, "ip", "link", "show", tapDev)
	if existsErr != nil {
		return
	}
	_ = n.executor.Run(ctx, "ip", "link", "set", tapDev, "nomaster")
	_ = n.executor.Run(ctx, "ip", "link", "delete", tapDev)
}

// SafeCleanupNATByComment removes all iptables nat rules tagged with the given
// comment string. Uses deterministic matching — no host-wide flushes.
func (n *NetworkManager) SafeCleanupNATByComment(ctx context.Context, comment string) {
	for _, chain := range []string{"PREROUTING", "POSTROUTING", "OUTPUT", "INPUT"} {
		out, err := n.executor.RunOutput(ctx, "iptables", "-t", "nat", "-L", chain, "-n", "-v", "--line-numbers")
		if err != nil {
			continue
		}
		lines := strings.Split(out, "\n")
		var nums []string
		for _, line := range lines {
			if strings.Contains(line, comment) {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					nums = append([]string{fields[0]}, nums...)
				}
			}
		}
		for _, num := range nums {
			_ = n.executor.Run(ctx, "iptables", "-t", "nat", "-D", chain, num)
		}
	}
}

// SafeCleanupFilterByComment removes all iptables filter table rules tagged
// with the given comment and deletes any custom chains with that prefix.
func (n *NetworkManager) SafeCleanupFilterByComment(ctx context.Context, comment string) {
	for _, chain := range []string{"FORWARD", "INPUT", "OUTPUT"} {
		out, err := n.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", chain, "-n", "-v", "--line-numbers")
		if err != nil {
			continue
		}
		lines := strings.Split(out, "\n")
		var nums []string
		for _, line := range lines {
			if strings.Contains(line, comment) {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					nums = append([]string{fields[0]}, nums...)
				}
			}
		}
		for _, num := range nums {
			_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-D", chain, num)
		}
	}

	out, err := n.executor.RunOutput(ctx, "iptables", "-t", "filter", "-L", "-n")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Chain "+comment) {
			chain := strings.TrimPrefix(trimmed, "Chain ")
			_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-F", chain)
			_ = n.executor.Run(ctx, "iptables", "-t", "filter", "-X", chain)
		}
	}
}

// SafeCleanupAll removes all TAP devices and iptables rules/chains tagged with
// deterministic cpvm-test identifiers. Does NOT flush global rulesets.
func (n *NetworkManager) SafeCleanupAll(ctx context.Context) {
	out, err := n.executor.RunOutput(ctx, "ip", "link", "show")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "tap-cpvm-te") {
			continue
		}
		fields := strings.SplitN(trimmed, ":", 2)
		if len(fields) < 2 {
			continue
		}
		dev := strings.TrimSpace(fields[1])
		if strings.HasPrefix(dev, "tap-cpvm-te") {
			_ = n.executor.Run(ctx, "ip", "link", "set", dev, "nomaster")
			_ = n.executor.Run(ctx, "ip", "link", "delete", dev)
		}
	}

	n.SafeCleanupNATByComment(ctx, "cpvm-test")
	n.SafeCleanupFilterByComment(ctx, "cpvm-test")
}

// ── Test helper constructors ──────────────────────────────────────────────────

// makeTestNM creates a NetworkManager with RealExecutor for privileged tests.
func makeTestNM() *NetworkManager {
	return NewNetworkManager(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// bridgeMaybeMissing returns true if the error looks like the bridge does not exist.
func bridgeMaybeMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sub := range []string{"bridge", "master", "No such device", "not found"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// mustBeLinuxOrSkip checks that the OS is Linux and VM_PLATFORM_ENABLE_NET_TESTS
// is set. Returns a non-empty skip message if conditions are not met.
func mustBeLinuxOrSkip() string {
	if runtime.GOOS != "linux" {
		return fmt.Sprintf("VM_PLATFORM_ENABLE_NET_TESTS privileged tests require Linux (current: %s)", runtime.GOOS)
	}
	if os.Getenv("VM_PLATFORM_ENABLE_NET_TESTS") != "1" {
		return "set VM_PLATFORM_ENABLE_NET_TESTS=1 to run privileged networking tests"
	}
	return ""
}
