// Package internal wires the CLI application and logging.
package internal

import (
	"context"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/imunhatep/aws-mcp-go/internal/version"
)

// NewApp builds the root CLI command. Subcommands are attached by main.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:                  "aws-mcp",
		Usage:                 "MCP server exposing AWS resources across accounts and regions to Claude Code",
		UsageText:             "aws-mcp [global options] command [command options]",
		Description:           "Lists AWS resources (EC2, S3, RDS, Glue, EMR, ...) across accounts and regions through awslib's parallel resource-fetching pipeline, with a pluggable cache.",
		Version:               version.Version,
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "log verbosity: 0=fatal 1=error 2=warn 3=info 4=debug 5=trace",
				Value:   3,
				Sources: cli.EnvVars("AWS_MCP_VERBOSE", "LOG_LEVEL"),
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			SetLogLevel(int(cmd.Int("verbose")))
			return ctx, nil
		},
	}
}

// SetLogLevel configures the global zerolog logger and level.
func SetLogLevel(level int) {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime})

	switch level {
	case 0:
		zerolog.SetGlobalLevel(zerolog.FatalLevel)
	case 1:
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	case 2:
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case 3:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case 4:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	}
}
