package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/protocol"
	safetycontrol "github.com/Jacko-John/cpa-usage-statistics/internal/safety"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func TestUsageSummaryWithoutStoreUsesRequestedRateMinutes(t *testing.T) {
	useGlobalSafetyStore(t, nil, nil)
	for _, test := range []struct {
		rangeName   string
		rateMinutes int64
	}{
		{rangeName: "7h", rateMinutes: 420},
		{rangeName: "24h", rateMinutes: 1440},
		{rangeName: "7d", rateMinutes: 10080},
		{rangeName: "30d", rateMinutes: 43200},
		{rangeName: "all"},
	} {
		t.Run(test.rangeName, func(t *testing.T) {
			body, err := json.Marshal(usageSummaryRequest{Range: test.rangeName})
			if err != nil {
				t.Fatal(err)
			}
			response := callManagementForTest(t, protocol.ManagementRequest{
				Method: "POST",
				Path:   "/v0/management" + managementUsagePath,
				Body:   body,
			})
			if response.StatusCode != 200 {
				t.Fatalf("status = %d, body=%s", response.StatusCode, response.Body)
			}
			summary := testutil.DecodeUsageSummary(t, response.Body)
			if summary.Range != test.rangeName || summary.RateMinutes != test.rateMinutes {
				t.Fatalf("summary = %+v", summary)
			}
		})
	}
}

func TestQueryRecentEnforcesMaximumAndOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for index := 0; index < 105; index++ {
		id := fmt.Sprintf("record-%03d", index)
		if err := store.Insert(ctx, usagemodel.Record{
			ID:        id,
			Timestamp: base.Add(time.Duration(index) * time.Second),
			APIKey:    "client-key",
			Model:     "model-a",
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	usage, err := store.QueryRecent(ctx, usagemodel.QueryRange{}, 1000)
	if err != nil {
		t.Fatalf("QueryRecent: %v", err)
	}
	details := usage["client-key"]["model-a"]
	if len(details) != 100 {
		t.Fatalf("recent details = %d, want 100", len(details))
	}
	if details[0].ID != "record-104" || details[len(details)-1].ID != "record-005" {
		t.Fatalf("recent order = %q ... %q", details[0].ID, details[len(details)-1].ID)
	}

	useGlobalSafetyStore(t, safety, store)
	response := callManagementForTest(t, protocol.ManagementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementUsagePath,
		Query:  map[string][]string{"limit": {"1000"}},
	})
	if response.StatusCode != 200 {
		t.Fatalf("management events status = %d body=%s", response.StatusCode, response.Body)
	}
	var managedUsage usagemodel.APIUsage
	if err := json.Unmarshal(response.Body, &managedUsage); err != nil {
		t.Fatalf("unmarshal management events: %v", err)
	}
	if got := len(managedUsage["client-key"]["model-a"]); got != 100 {
		t.Fatalf("management events = %d, want 100", got)
	}

	body, err := json.Marshal(usageSummaryRequest{Range: "all"})
	if err != nil {
		t.Fatalf("marshal summary request: %v", err)
	}
	response = callManagementForTest(t, protocol.ManagementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementUsagePath,
		Body:   body,
	})
	if response.StatusCode != 200 {
		t.Fatalf("management summary status = %d body=%s", response.StatusCode, response.Body)
	}
	if got := testutil.DecodeUsageSummary(t, response.Body).TotalRequests; got != 105 {
		t.Fatalf("management summary requests = %d, want 105", got)
	}
}

func TestUsagePricingManagementRoutes(t *testing.T) {
	var methods []string
	for _, route := range managementRegistration().Routes {
		if route.Path == managementPricingPath {
			methods = append(methods, route.Method)
		}
	}
	if len(methods) != 2 || methods[0] != "GET" || methods[1] != "PUT" {
		t.Fatalf("pricing methods = %v", methods)
	}

	store := newTestStore(t)
	if err := store.Insert(t.Context(), usagemodel.Record{
		ID:        "record",
		Timestamp: time.Now().Add(-time.Minute),
		Model:     "model-a",
		Tokens:    usagemodel.TokenStats{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	useGlobalSafetyStore(t, safetycontrol.New(), store)
	pricing := statistics.Pricing{Prices: map[string]statistics.ModelPrice{"model-a": {Input: 2}}}
	body, err := json.Marshal(pricing)
	if err != nil {
		t.Fatalf("marshal pricing: %v", err)
	}
	response := callManagementForTest(t, protocol.ManagementRequest{
		Method: "PUT",
		Path:   "/v0/management" + managementPricingPath,
		Body:   body,
	})
	if response.StatusCode != 200 {
		t.Fatalf("pricing PUT status = %d body=%s", response.StatusCode, response.Body)
	}
	var updated statistics.Pricing
	if err := json.Unmarshal(response.Body, &updated); err != nil {
		t.Fatalf("unmarshal pricing PUT: %v", err)
	}
	_, wantKey := statistics.NormalizePricing(pricing)
	_, updatedKey := statistics.NormalizePricing(updated)
	if updatedKey != wantKey {
		t.Fatalf("pricing PUT response = %+v", updated)
	}

	response = callManagementForTest(t, protocol.ManagementRequest{
		Method: "GET",
		Path:   "/v0/management" + managementPricingPath,
	})
	if response.StatusCode != 200 {
		t.Fatalf("pricing GET status = %d body=%s", response.StatusCode, response.Body)
	}
	var loaded statistics.Pricing
	if err := json.Unmarshal(response.Body, &loaded); err != nil {
		t.Fatalf("unmarshal pricing GET: %v", err)
	}
	_, loadedKey := statistics.NormalizePricing(loaded)
	if loadedKey != wantKey {
		t.Fatalf("pricing GET response = %+v", loaded)
	}

	summaryBody, err := json.Marshal(usageSummaryRequest{Range: "all"})
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	response = callManagementForTest(t, protocol.ManagementRequest{
		Method: "POST",
		Path:   "/v0/management" + managementUsagePath,
		Body:   summaryBody,
	})
	if response.StatusCode != 200 {
		t.Fatalf("summary status = %d body=%s", response.StatusCode, response.Body)
	}
	testutil.AssertCost(t, testutil.DecodeUsageSummary(t, response.Body).TotalCost, 2)
}
