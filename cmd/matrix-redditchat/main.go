package main

import (
	"os"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/ricardo-duarte-av/matrix-redditchat-bridge/pkg/connector"
)

// Filled at build time with -X linker flags, see build.sh.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var m = mxmain.BridgeMain{
	Name:        "matrix-redditchat-bridge",
	Description: "A Matrix-Reddit chat puppeting bridge.",
	URL:         "https://github.com/ricardo-duarte-av/matrix-redditchat-bridge",
	Version:     "0.1.0",
	Connector:   connector.NewConnector(),
}

func main() {
	m.AdditionalLongFlags = " [--generate-doublepuppet-registration]"
	m.InitVersion(Tag, Commit, BuildTime)

	// Run() is expanded here so the double puppet registration can be generated after the
	// config is loaded but before the database is opened and the bridge starts.
	m.PreInit()
	if *generateDoublePuppetRegistration {
		generateDoublePuppetRegistrationFile()
		os.Exit(0)
	}
	m.Init()
	m.Start()
	exitCode := m.WaitForInterrupt()
	m.Stop()
	os.Exit(exitCode)
}
