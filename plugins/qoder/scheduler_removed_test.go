package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestRegistrationDoesNotAdvertiseScheduler(t *testing.T) {
	raw, err := json.Marshal(wbRegistration())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"scheduler"`)) {
		t.Fatalf("registration still advertises scheduler: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"scheduler_mode"`)) {
		t.Fatalf("registration still advertises scheduler_mode: %s", raw)
	}
}

func TestHandleMethodSchedulerPickIsUnknown(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodSchedulerPick, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("scheduler.pick should be unknown after routing ownership moves to CPA: %s", raw)
	}
}
