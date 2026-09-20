package install

import (
	"context"
	"strings"
	"testing"
)

func TestStoreKeychainSecretUsesSilentPTYWithoutSecretArguments(t *testing.T) {
	r := NewFakeRunner()
	const secret = "fake-secret-for-test"
	if err := StoreKeychainSecret(context.Background(), r, "device-1", "grove.test", secret); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("/usr/bin/security delete-generic-password -a device-1 -s grove.test") {
		t.Fatal("stale item was not deleted")
	}
	for _, call := range r.Calls {
		if strings.Contains(call.String(), secret) {
			t.Fatalf("secret appeared in argv: %s", call.String())
		}
		if call.Name == "/usr/bin/expect" {
			if call.Stdin != "device-1\ngrove.test\n"+secret+"\n" {
				t.Fatal("expect did not receive credential through stdin")
			}
			if strings.Contains(call.String(), "-U") || strings.Contains(call.String(), "-A") || strings.Contains(call.String(), "-T") {
				t.Fatalf("insecure or prompt-causing Keychain ACL flag used: %s", call.String())
			}
		}
	}
}

func TestStoreKeychainSecretRejectsMultilineFields(t *testing.T) {
	r := NewFakeRunner()
	if err := StoreKeychainSecret(context.Background(), r, "device-1", "grove.test", "bad\nsecret"); err == nil {
		t.Fatal("expected multiline secret rejection")
	}
	if len(r.Calls) != 0 {
		t.Fatal("invalid fields must be rejected before invoking Keychain commands")
	}
}
