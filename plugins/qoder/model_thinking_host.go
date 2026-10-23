package main

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

// thinkingSupportForHost projects parsed upstream thinking facts onto the host
// ModelInfo contract. Only a confirmed supported status with named levels is
// advertised: unknown/unsupported/off-only stay nil so CPA does not invent
// reasoning controls the gateway never offered. Levels are copied because the
// host clones the pointer but callers must not alias our catalog slice.
func thinkingSupportForHost(f *thinkingFacts) *pluginapi.ThinkingSupport {
	if f == nil || f.Status != "supported" || len(f.Levels) == 0 {
		return nil
	}
	levels := append([]string(nil), f.Levels...)
	out := &pluginapi.ThinkingSupport{Levels: levels}
	if f.CanDisable != nil {
		out.ZeroAllowed = *f.CanDisable
	}
	return out
}
