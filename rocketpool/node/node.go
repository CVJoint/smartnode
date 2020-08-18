package node

import (
    "github.com/urfave/cli"
)


// Register node command
func RegisterCommands(app *cli.App, name string, aliases []string) {
    app.Commands = append(app.Commands, cli.Command{
        Name:      name,
        Aliases:   aliases,
        Usage:     "Run Rocket Pool node activity daemon",
        Action: func(c *cli.Context) error {
            return run(c)
        },
    })
}


// Run daemon
func run(c *cli.Context) error {

    // Initialize tasks
    stakePrelaunchMinipools, err := newStakePrelaunchMinipools(c)
    if err != nil { return err }

    // Start tasks
    stakePrelaunchMinipools.Start()

    // Block thread
    select {}

}
