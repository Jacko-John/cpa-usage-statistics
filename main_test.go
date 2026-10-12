package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Jacko-John/cpa-usage-statistics/internal/plugin"
	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
)

// TestRegistryMatchesHostRegistration checks the catalog against the executable entry point.
func TestRegistryMatchesHostRegistration(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID, Name, Author, Version, Repository, Homepage string
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if registry.SchemaVersion != 1 || len(registry.Plugins) != 1 {
		t.Fatalf("unexpected registry: %+v", registry)
	}
	entry := registry.Plugins[0]
	if entry.ID != protocol.PluginID {
		t.Fatalf("registry ID = %q, protocol ID = %q", entry.ID, protocol.PluginID)
	}
	if entry.Homepage != "https://github.com/Jacko-John/Cli-Proxy-API-Management-Center" {
		t.Fatalf("frontend repository = %q", entry.Homepage)
	}

	// Register through the same dispatcher used by the C ABI, without touching real data.
	request, err := json.Marshal(map[string]any{
		"schema_version": 4,
		"config_yaml":    fmt.Sprintf("data_dir: %q\n", t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plugin.Close)
	response, err := handleMethod("plugin.register", request)
	if err != nil {
		t.Fatal(err)
	}
	var registration struct {
		OK     bool `json:"ok"`
		Result struct {
			Metadata struct {
				Name, Version, Author, GitHubRepository string
			}
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &registration); err != nil {
		t.Fatal(err)
	}
	metadata := registration.Result.Metadata
	if !registration.OK || metadata.Name != entry.Name || metadata.Version != entry.Version ||
		metadata.Author != entry.Author || metadata.GitHubRepository != entry.Repository {
		t.Fatalf("host metadata = %+v, registry entry = %+v", metadata, entry)
	}

	// Every advertised management route belongs to the independent installation ID.
	response, err = handleMethod("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var management struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct{ Method, Path string } `json:"routes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &management); err != nil {
		t.Fatal(err)
	}
	if !management.OK || len(management.Result.Routes) == 0 {
		t.Fatalf("missing management routes: %s", response)
	}
	for _, route := range management.Result.Routes {
		if !strings.HasPrefix(route.Path, "/plugins/"+entry.ID+"/") {
			t.Fatalf("route outside plugin namespace: %+v", route)
		}
	}
}
