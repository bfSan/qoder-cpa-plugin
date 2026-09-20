package main

import (
	"reflect"
	"testing"
)

func TestSortModelsForCatalogGroupsAggregatesGPTThenFamilies(t *testing.T) {
	base := testModels(
		"gpt-6-astra",
		"qmodel",
		"deep-model",
		"gpt-5.5",
		"kmodel",
		"auto",
		"qwen3.8-flash",
		"gmodel",
	)
	got := modelIDs(sortModelsForCatalog(base))
	want := []string{
		"auto",
		"deep-model",
		"gpt-5.5",
		"gpt-6-astra",
		"gmodel",
		"kmodel",
		"qmodel",
		"qwen3.8-flash",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted = %v, want %v", got, want)
	}
}

func TestSortModelsForCatalogIsStableWithinUnknownFamily(t *testing.T) {
	base := testModels("zzz", "aaa", "bbb")
	got := modelIDs(sortModelsForCatalog(base))
	if want := []string{"aaa", "bbb", "zzz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted = %v, want %v", got, want)
	}
}
