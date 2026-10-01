package main

import "github.com/Jacko-John/cpa-usage-statistics/internal/plugin"

const abiVersion uint32 = 1

// 可通过 -ldflags "-X main.pluginVersion=..." 覆盖。
var (
	pluginVersion    = "0.3.1"
	pluginAuthor     = "Fwindy"
	pluginRepository = "https://github.com/Jacko-John/cpa-usage-statistics"
)

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	return plugin.Handle(method, request, plugin.Metadata{
		Version: pluginVersion, Author: pluginAuthor, Repository: pluginRepository,
	})
}
