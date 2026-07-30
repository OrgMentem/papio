// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// A router method may use a distinct transport without a one-to-one CLI command,
// but it may not expose a domain fact or domain transition with no Cobra reachability.
package cli

import (
	"sort"
	"strings"
	"testing"

	"papio/internal/api"
)

// rpcOnlyInfrastructure names transport machinery that deliberately has no Cobra
// command. Each exception must explain why it cannot carry a domain fact or
// transition that belongs on the CLI surface.
var rpcOnlyInfrastructure = map[string]string{
	"browser.sync": "The native-messaging host uses this extension handshake to publish browser session state; browser sessions and browser use expose the resulting user-visible state.",
}

func TestEveryDomainRPCIsReachableFromCLI(t *testing.T) {
	// Router construction only closes over system; handlers dereference it when
	// invoked, which this surface-only test never does. Supplying shutdown includes
	// daemon.shutdown, so the served set matches the daemon rather than its
	// shutdown-less construction helper.
	router := api.RouterWithShutdown(nil, func() {})
	served := router.Methods

	reachable := make(map[string]struct{})
	for command, class := range commandClassification {
		for _, method := range class.rpcMethods {
			reachable[method] = struct{}{}
			if _, ok := served[method]; !ok {
				t.Errorf("%s claims RPC method %q, but the live router does not serve it; update the command classification after the wire surface changes", command, method)
			}
		}
	}

	for method, rationale := range rpcOnlyInfrastructure {
		if strings.TrimSpace(rationale) == "" {
			t.Errorf("rpcOnlyInfrastructure[%q] has no rationale; ADR-0001 permits only a justified transport/lifecycle exception", method)
		}
		if _, ok := served[method]; !ok {
			t.Errorf("rpcOnlyInfrastructure names %q, but the live router does not serve it; remove the stale exception", method)
		}
	}

	unreachable := make([]string, 0)
	for method := range served {
		if _, ok := reachable[method]; ok {
			continue
		}
		if _, ok := rpcOnlyInfrastructure[method]; ok {
			continue
		}
		unreachable = append(unreachable, method)
	}
	sort.Strings(unreachable)
	for _, method := range unreachable {
		t.Errorf("router method %q has no Cobra reachability, violating ADR-0001; add a CLI command or add an exact, justified rpcOnlyInfrastructure entry", method)
	}
}
