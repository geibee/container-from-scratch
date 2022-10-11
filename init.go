package main

import (
	"regmarmcem/container-from-scratch/container"

	"github.com/urfave/cli/v2"
)

var initCommand = &cli.Command{
	Name:        "init",
	Usage:       "initialize a container",
	Description: "initialize a container",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "command",
		},
	},
	Action: func(ctx *cli.Context) error {
		container.Initialization(ctx)
		return nil
	},
}
