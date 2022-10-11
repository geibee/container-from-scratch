package main

import (
	"regmarmcem/container-from-scratch/container"

	"github.com/urfave/cli/v2"
)

var runCommand = &cli.Command{
	Name:        "run",
	Usage:       "run a container",
	Description: `Run a container.`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "command",
		},
	},
	Action: func(ctx *cli.Context) error {
		container.Run(ctx)
		return nil
	},
}
