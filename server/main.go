// Command kandev-plugin-discord is the backend half of the Discord plugin.
// Kandev spawns it as a go-plugin gRPC subprocess; pluginsdk.Serve owns the
// transport.
package main

import "github.com/kdlbs/kandev/pluginsdk"

func main() {
	pluginsdk.Serve(newDiscordPlugin())
}
