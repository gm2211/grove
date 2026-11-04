package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRedactedHidesEverySecret(t *testing.T) {
	cfg := &Config{}
	// Fill every string field tagged secret with a recognizable value, found by reflection so a
	// newly tagged field is covered automatically.
	var secrets []string
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			fv := v.Field(i)
			if f.Tag.Get("secret") == "true" {
				val := "SECRETVALUE-" + f.Name
				fv.SetString(val)
				secrets = append(secrets, val)
			} else if fv.Kind() == reflect.Struct {
				fill(fv)
			}
		}
	}
	fill(reflect.ValueOf(cfg).Elem())
	if len(secrets) < 6 {
		t.Fatalf("found only %d secret fields: %v", len(secrets), secrets)
	}
	cfg.Orchard.URL = "http://orchard"

	out, err := yaml.Marshal(Redacted(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(out), s) {
			t.Errorf("redacted output contains %q:\n%s", s, out)
		}
	}
	if !strings.Contains(string(out), "<set, len ") || !strings.Contains(string(out), "http://orchard") {
		t.Errorf("redacted output lost its shape:\n%s", out)
	}
	if cfg.Nomad.Token != "SECRETVALUE-Token" {
		t.Error("Redacted mutated its input")
	}
}

func TestSecretKeys(t *testing.T) {
	for _, k := range []string{"nomad.token", "orchard.token", "server.token", "enrollment.issuerToken"} {
		if !IsSecretKey(k) {
			t.Errorf("%s not recognised as secret", k)
		}
	}
	for _, k := range []string{"nomad.url", "server.tokenKeychain", "nomad.clientKey"} {
		if IsSecretKey(k) {
			t.Errorf("%s wrongly secret", k)
		}
	}
}

func TestLoadFromRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := dir + "/real.yaml"
	if err := Save(real, &Config{}); err != nil {
		t.Fatal(err)
	}
	link := dir + "/config.yaml"
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, err := LoadFrom(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink refusal", err)
	}
}
