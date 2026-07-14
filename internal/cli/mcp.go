// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"github.com/spf13/cobra"

	"papio/internal/bootstrap"
	"papio/internal/mcpserver"
)

func newMCPCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve Papio tools and resources over MCP stdio",
		Args:  cobra.NoArgs,
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
			return mcpserver.Run(cmd.Context(), system)
		},
	}
}
