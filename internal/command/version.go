// Package command defines the urfave/cli command definitions.
package command

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/imunhatep/aws-mcp-go/internal/version"
)

// VersionCommand prints build metadata.
type VersionCommand struct{}

func (c VersionCommand) Command() *cli.Command {
	return &cli.Command{
		Name:   "version",
		Usage:  "print version and commit",
		Action: c.run,
	}
}

func (c VersionCommand) run(_ context.Context, _ *cli.Command) error {
	fmt.Printf("aws-mcp %s (%s)\n", version.Version, version.Commit)
	return nil
}
