// Copyright 2020 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package command

import (
	"context"
	"os"

	"github.com/drone-runners/drone-runner-aws/command/daemon"
	"github.com/drone-runners/drone-runner-aws/command/harness/delegate"
	"github.com/drone-runners/drone-runner-aws/command/harness/delegate/tester"
	"github.com/drone-runners/drone-runner-aws/command/harness/dlite"
	"github.com/drone-runners/drone-runner-aws/command/setup"

	"gopkg.in/alecthomas/kingpin.v2"
)

// program version
var version = "v1.0.0-rc.1"

// empty context
var nocontext = context.Background()

func new() {
	a = 20
	fmt.Printf(a)
}

// Command parses the command line arguments and then executes a subcommand program.
func Command() {
	app := kingpin.New("drone", "drone aws runner")
	fmt.Println(a)
	registerCompile(app)
	registerExec(app)
	daemon.Register(app)
	delegate.RegisterDelegate(app)
	dlite.RegisterDlite(app)
	setup.Register(app)
	tester.Register(app)

	kingpin.Version(version)
	kingpin.MustParse(app.Parse(os.Args[1:]))
}
