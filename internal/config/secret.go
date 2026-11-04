package config

import (
	"fmt"
	"reflect"
	"strings"
)

// A Config field holding a secret carries the struct tag `secret:"true"`. Everything that must
// treat secrets differently (`grove config show`, `grove config set`'s argv warning) is derived
// from the tags, so a new secret field only needs the tag.

// SecretKeys returns the dotted config keys (as `grove config set` names them, e.g.
// "nomad.token") of every field tagged secret:"true", sorted by declaration order.
func SecretKeys() []string {
	var keys []string
	collectSecretKeys(reflect.TypeOf(Config{}), "", &keys)
	return keys
}

// IsSecretKey reports whether the dotted config key names a secret field.
func IsSecretKey(key string) bool {
	for _, k := range SecretKeys() {
		if k == key {
			return true
		}
	}
	return false
}

func yamlName(f reflect.StructField) string {
	name := strings.Split(f.Tag.Get("yaml"), ",")[0]
	if name == "" || name == "-" {
		return ""
	}
	return name
}

func collectSecretKeys(t reflect.Type, prefix string, keys *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := yamlName(f)
		if name == "" {
			continue
		}
		path := prefix + name
		switch {
		case f.Tag.Get("secret") == "true" && f.Type.Kind() == reflect.String:
			*keys = append(*keys, path)
		case f.Type.Kind() == reflect.Struct:
			collectSecretKeys(f.Type, path+".", keys)
		}
	}
}

// Redacted returns a deep copy of cfg in which every non-empty secret field is replaced by
// "<set, len N>". An empty secret stays empty, so the output still shows which are unset.
func Redacted(cfg *Config) *Config {
	out := *cfg
	redactStruct(reflect.ValueOf(&out).Elem())
	return &out
}

func redactStruct(v reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fv := v.Field(i)
		switch {
		case f.Tag.Get("secret") == "true" && fv.Kind() == reflect.String:
			if n := len(fv.String()); n > 0 {
				fv.SetString(fmt.Sprintf("<set, len %d>", n))
			}
		case fv.Kind() == reflect.Struct:
			redactStruct(fv)
		}
	}
}
