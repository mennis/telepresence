//go:build !windows
// +build !windows

package agentinit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"

	"github.com/coreos/go-iptables/iptables"

	"github.com/telepresenceio/telepresence/v2/pkg/install"
)

const nat = "nat"
const inboundChain = "TEL_INBOUND"

type config struct {
	AgentPort     int
	AppPort       int
	AgentProtocol string
}

func (c *config) validate() error {
	if c == nil {
		return errors.New("nil config entry")
	}
	if c.AgentPort <= 0 {
		return errors.New("invalid AgentPort")
	}
	if c.AppPort <= 0 {
		return errors.New("invalid AppPort")
	}
	if c.AgentProtocol == "" {
		return errors.New("invalid AgentProtocol")
	}
	return nil
}

func configureIptables(ctx context.Context, iptables *iptables.IPTables, loopback string, cfgs ...*config) error {
	// These iptables rules implement routing such that a packet directed to the appPort will hit the agentPort instead.
	// If there's no mesh this is simply request -> agent -> app (or intercept)
	// However, if there's a service mesh we want to make sure we don't bypass the mesh, so the traffic will flow request -> mesh -> agent -> app
	agentUID := strconv.FormatInt(install.AgentUID, 10)
	// Clearing the inbound chain will create it if it doesn't exist, or clear it out if it does.
	err := iptables.ClearChain(nat, inboundChain)
	if err != nil {
		return fmt.Errorf("failed to clear chain %s: %w", inboundChain, err)
	}
	// Use our inbound chain to direct traffic coming into the app port to the agent port.
	for _, cfg := range cfgs {
		appPort := strconv.Itoa(cfg.AppPort)
		agentPort := strconv.Itoa(cfg.AgentPort)
		err = iptables.AppendUnique(nat, inboundChain,
			"-p", cfg.AgentProtocol, "--dport", appPort,
			"-j", "REDIRECT", "--to-ports", agentPort)
		if err != nil {
			return fmt.Errorf("failed to append rule to %s: %w", inboundChain, err)
		}
		// Direct everything coming into PREROUTING into our own inbound chain.
		// We do this as an append instead of an insert because this will prevent us from interfering with a service mesh
		// if one exists. If a service mesh exists, its PREROUTING rules will kick in before ours, ensuring traffic
		// coming into the pod does not bypass the mesh.
		err = iptables.AppendUnique(nat, "PREROUTING",
			"-p", cfg.AgentProtocol,
			"-j", inboundChain)
		if err != nil {
			return fmt.Errorf("failed to append prerouting rule to direct to %s: %w", inboundChain, err)
		}
	}
	// Any traffic heading out of the loopback and into the app port (other than traffic from the agent) needs to
	// be redirected to the agent. This will ensure that if there's a service mesh, when the mesh's proxy goes to
	// request the application, it will get a response via the traffic agent.
	err = iptables.Insert(nat, "OUTPUT", 1, "-o", loopback,
		"-m", "owner", "!", "--gid-owner", agentUID,
		"-j", inboundChain)
	if err != nil {
		return fmt.Errorf("failed to insert ! --gid-owner rule in OUTPUT: %w", err)
	}
	// Any agent traffic heading out on the loopback but NOT towards localhost needs to be processed in case
	// it needs to be redirected. This is so that if the traffic agent requests its own IP, it doesn't just
	// serve the app but actually goes through the agent, and thus through any intercepts.
	// This is needed to support requesting an intercepted pod by IP (or to intercept a headless service).
	for _, cfg := range cfgs {
		err = iptables.Insert(nat, "OUTPUT", 1,
			"-o", loopback,
			"-p", cfg.AgentProtocol,
			"!", "-d", "127.0.0.1/32",
			"-m", "owner", "--gid-owner", agentUID,
			"-j", inboundChain)
		if err != nil {
			return fmt.Errorf("failed to insert --gid-owner rule in OUTPUT: %w", err)
		}
	}
	// Finally, any other traffic heading out of the traffic agent should pass by unperturbed -- it should obviously not be
	// redirected back into the agent, but it also should not pass through a mesh proxy.
	// This will include not just agent->manager traffic but also the agent requesting 127.0.0.1:appPort to serve the application
	err = iptables.Insert(nat, "OUTPUT", 2,
		"-m", "owner", "--gid-owner", agentUID,
		"-j", "RETURN")
	if err != nil {
		return fmt.Errorf("failed to insert --gid-owner rule in OUTPUT: %w", err)
	}
	return nil
}

func findLoopback(ctx context.Context) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to get network interfaces: %w", err)
	}
	for _, iface := range ifaces {
		// If the interface is down, we can't use it anyway
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			return iface.Name, nil
		}
	}
	return "", fmt.Errorf("unable to find loopback network interface")
}

// Main is the main function for the agent init container
func Main(ctx context.Context, args ...string) (err error) {
	evRx := regexp.MustCompile(`^([A-Z_]+)_(\d+)=(.*)$`)
	cfgs := make([]*config, 0)
	cfgAt := func(i int) *config {
		for i >= len(cfgs) {
			cfgs = append(cfgs, &config{})
		}
		return cfgs[i]
	}
	for _, ep := range os.Environ() {
		if m := evRx.FindStringSubmatch(ep); m != nil {
			key := m[1]
			ix, _ := strconv.Atoi(m[2])
			value := m[3]
			switch key {
			case "AGENT_PORT":
				if cfgAt(ix).AgentPort, err = strconv.Atoi(value); err != nil {
					return fmt.Errorf("AGENT_PORT must be an integer but was %q", value)
				}
			case "APP_PORT":
				if cfgAt(ix).AppPort, err = strconv.Atoi(value); err != nil {
					return fmt.Errorf("APP_PORT must be an integer but was %q", value)
				}
			case "AGENT_PROTOCOL":
				cfgAt(ix).AgentProtocol = value
			}
		}
	}
	for _, cfg := range cfgs {
		if err = cfg.validate(); err != nil {
			return err
		}
	}

	lo, err := findLoopback(ctx)
	if err != nil {
		return err
	}
	it, err := iptables.New()
	if err != nil {
		return fmt.Errorf("unable to create iptables instance: %w", err)
	}
	return configureIptables(ctx, it, lo, cfgs...)
}
