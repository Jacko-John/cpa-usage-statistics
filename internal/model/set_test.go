package model

import (
	"slices"
	"testing"
)

func TestOrderedStringSetIsUniqueAndStable(t *testing.T) {
	set := NewStableStringSet(4)
	for _, value := range []string{"first", "second", "first", "third"} {
		set.Add(value)
	}
	if want := []string{"first", "second", "third"}; !slices.Equal(set.Values(), want) {
		t.Fatalf("values = %v, want %v", set.Values(), want)
	}
}
