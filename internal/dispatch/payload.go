package dispatch

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// The Nomad dispatch payload is the one part of a dispatch no task sees unless it declares
// dispatch_payload, so it is where everything secret travels: the private-repo clone token and
// the values of the job's named secrets. Dispatch meta is in the environment of every task of the
// job; nothing secret goes there.
//
// Three forms exist:
//
//	<script>                                          a shell job with no named secret
//	grove-payload/1\n<token>\n<script>                a build or agent job with no named secret
//	grove-payload/2\n<token>\n<secrets>\n<script>     any job with named secrets
//
// <token> is the clone token or an empty line. <secrets> is one line: the standard base64 of a
// JSON object of name to value, so a value may hold any text, newlines included. Version 2 is
// sent only when a job names a secret, so everything else reads exactly as it did before version
// 2 existed. The readers are the "source" task of build.nomad.hcl and agent.nomad.hcl, run.sh of
// shell.nomad.hcl, and DecodePayload for sealed pools (internal/sandbox).
const (
	payloadHeader        = "grove-payload/1\n"
	payloadHeaderSecrets = "grove-payload/2\n"
	// payloadPrefix starts every framed payload. A shell job's script may not start with it,
	// since its payload is read as a frame when it does.
	payloadPrefix = "grove-payload/"
)

// MaxPayloadBytes is the largest dispatch payload Nomad accepts (its fixed 16 KiB limit).
const MaxPayloadBytes = 16 * 1024

// Payload is what a dispatch payload carries.
type Payload struct {
	// CloneToken is the armed private-repo credential, "" for none. Build and agent jobs only.
	CloneToken string
	// Secrets holds the values of the job's named secrets by name.
	Secrets map[string]string
	Script  string
}

// encodePayload builds the Nomad dispatch payload for kind (see the forms above).
func encodePayload(kind Kind, p Payload) ([]byte, error) {
	framed := kind == KindBuild || kind == KindAgent
	if !framed && p.CloneToken != "" {
		return nil, fmt.Errorf("dispatch: a %s job has no clone token", kind)
	}
	if len(p.Secrets) > 0 {
		// encoding/json writes map keys sorted, so the line is deterministic.
		raw, err := json.Marshal(p.Secrets)
		if err != nil {
			return nil, fmt.Errorf("dispatch: encode secrets: %w", err)
		}
		line := base64.StdEncoding.EncodeToString(raw)
		return []byte(payloadHeaderSecrets + p.CloneToken + "\n" + line + "\n" + p.Script), nil
	}
	if !framed {
		return []byte(p.Script), nil
	}
	return []byte(payloadHeader + p.CloneToken + "\n" + p.Script), nil
}

// DecodePayload reads a dispatch payload of a job of the given kind ("build", "agent" or
// "shell"). A payload with no header it knows is the script itself, as a shell job's always was.
// A secrets line that can't be read is an error: the job must not run without its secrets. The
// error never quotes the payload.
func DecodePayload(kind string, raw []byte) (Payload, error) {
	s := string(raw)
	switch {
	case strings.HasPrefix(s, payloadHeaderSecrets):
		rest := strings.TrimPrefix(s, payloadHeaderSecrets)
		token, rest, _ := strings.Cut(rest, "\n")
		line, script, _ := strings.Cut(rest, "\n")
		p := Payload{CloneToken: strings.TrimSuffix(token, "\r"), Script: script}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return p, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return Payload{}, fmt.Errorf("dispatch: the payload's secrets line is not base64")
		}
		if err := json.Unmarshal(decoded, &p.Secrets); err != nil {
			return Payload{}, fmt.Errorf("dispatch: the payload's secrets line is not a JSON object of strings")
		}
		return p, nil
	case kind != string(KindShell) && strings.HasPrefix(s, payloadHeader):
		token, script, _ := strings.Cut(strings.TrimPrefix(s, payloadHeader), "\n")
		return Payload{CloneToken: strings.TrimSuffix(token, "\r"), Script: script}, nil
	default:
		return Payload{Script: s}, nil
	}
}

// secretsOutOfMeta moves the named secrets a dispatch still carries in its env_json meta into its
// payload. Only a job dispatched by a grove from before payload version 2 has any there; a
// redispatch must not put them into meta a second time. names are the job's secret names (the
// record keeps those, never values). It returns its inputs unchanged when there is nothing to
// move, and never modifies the map it was given.
func secretsOutOfMeta(kind Kind, names []string, meta map[string]string, payload []byte) (map[string]string, []byte, error) {
	if len(names) == 0 || meta["env_json"] == "" {
		return meta, payload, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(meta["env_json"]), &env); err != nil {
		return nil, nil, fmt.Errorf("dispatch: the job's env_json is not a JSON object of strings")
	}
	moved := map[string]string{}
	for _, name := range names {
		if v, ok := env[name]; ok {
			moved[name] = v
			delete(env, name)
		}
	}
	if len(moved) == 0 {
		return meta, payload, nil
	}
	p, err := DecodePayload(string(kind), payload)
	if err != nil {
		return nil, nil, err
	}
	for name, v := range p.Secrets {
		if _, ok := moved[name]; !ok {
			moved[name] = v
		}
	}
	p.Secrets = moved
	out, err := encodePayload(kind, p)
	if err != nil {
		return nil, nil, err
	}
	if len(out) > MaxPayloadBytes {
		return nil, nil, fmt.Errorf("dispatch: the job's script and named secrets are %d bytes together; a dispatch carries at most %d", len(out), MaxPayloadBytes)
	}
	newMeta := make(map[string]string, len(meta))
	for k, v := range meta {
		newMeta[k] = v
	}
	if len(env) == 0 {
		delete(newMeta, "env_json")
	} else {
		envJSON, err := json.Marshal(env)
		if err != nil {
			return nil, nil, fmt.Errorf("dispatch: marshal env: %w", err)
		}
		newMeta["env_json"] = string(envJSON)
	}
	return newMeta, out, nil
}
