package notifications

import (
	"context"

	"github.com/Jacko-John/cpa-usage-statistics/internal/testfixture"
)

type testStore struct {
	*testfixture.Store
	*Service
}

func openTestStore(path string) (*testStore, error) {
	store, err := testfixture.Open(path)
	if err != nil {
		return nil, err
	}
	service, err := New(context.Background(), store.Store, nil)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &testStore{Store: store, Service: service}, nil
}
