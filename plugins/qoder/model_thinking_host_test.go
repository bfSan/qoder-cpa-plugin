package main

import (
	"reflect"
	"testing"
)

func TestThinkingSupportForHostOnlyConfirmedLevels(t *testing.T) {
	yes := true
	got := thinkingSupportForHost(&thinkingFacts{Status: "supported", Levels: []string{"low", "high"}, CanDisable: &yes})
	if got == nil || !reflect.DeepEqual(got.Levels, []string{"low", "high"}) || !got.ZeroAllowed {
		t.Fatalf("unexpected support: %+v", got)
	}
	for name, f := range map[string]*thinkingFacts{
		"nil":         nil,
		"unknown":     {Status: "unknown"},
		"unsupported": {Status: "unsupported"},
		"off only":    {Status: "supported", CanDisable: &yes},
	} {
		if thinkingSupportForHost(f) != nil {
			t.Fatalf("%s: invented support", name)
		}
	}
}

func TestThinkingSupportForHostDoesNotAliasLevels(t *testing.T) {
	f := &thinkingFacts{Status: "supported", Levels: []string{"low", "high"}}
	got := thinkingSupportForHost(f)
	got.Levels[0] = "mutated"
	if f.Levels[0] != "low" {
		t.Fatal("levels alias source")
	}
}
