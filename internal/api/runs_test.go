package api

import (
	"testing"

	"github.com/pancir/poligon/internal/model"
)

func TestParseEnv(t *testing.T) {
	env, err := parseEnv([]string{"PHONE=9525115368", "COMMENT=a=b c"})
	if err != nil || env["PHONE"] != "9525115368" || env["COMMENT"] != "a=b c" {
		t.Fatalf("got %v, %v", env, err)
	}
	for _, bad := range []string{"NOEQUALS", "1BAD=x", "POLIGON_DEVICE_ID=x", "=x"} {
		if _, err := parseEnv([]string{bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// Env values are often test credentials: responses and callbacks must not
// carry them, and redacting must not touch the stored spec.
func TestRedactedHidesEnv(t *testing.T) {
	run := model.Run{Spec: model.RunSpec{
		FlowPath: "/srv/storage/uploads/x/workspace/.maestro/flows/a.yaml",
		Env:      map[string]string{"CODE": "0000"},
	}}
	out := run.Redacted()
	if out.Spec.Env["CODE"] != "***" || out.Spec.FlowPath != "a.yaml" {
		t.Fatalf("redacted: %+v", out.Spec)
	}
	if run.Spec.Env["CODE"] != "0000" {
		t.Fatal("Redacted mutated the original env")
	}
}
