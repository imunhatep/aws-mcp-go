package main

import (
	"context"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/imunhatep/aws-mcp-go/internal"
	"github.com/imunhatep/aws-mcp-go/internal/command"
)

func main() {
	app := internal.NewApp()
	app.Commands = []*cli.Command{
		command.ServeCommand{}.Command(),
		command.VersionCommand{}.Command(),
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Error().Err(err).Msg("[aws-mcp]")
		os.Exit(1)
	}
}
