// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"io"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/bootstrap"
	"papio/internal/mcpserver"
)

func newMCPCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "mcp",
		Short:       "Serve papio tools and resources over MCP stdio",
		Annotations: map[string]string{"mcp:hidden": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			system, err := bootstrap.New(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer system.Close()
			call := api.InProcessCaller(system)
			factory := func(out, errOut io.Writer) *cobra.Command {
				return NewInProcessRoot(out, errOut, cfg, call)
			}
			return mcpserver.Run(cmd.Context(), system, factory)
		},
	}
}
