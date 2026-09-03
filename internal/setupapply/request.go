// Package setupapply holds the pure file semantics of the native guided
// setup launch: request validation, bounded reads, merge-by-UUID player
// lists, and the key-preserving server.properties patch. It deliberately
// never imports package server so it unit-tests in isolation, mirroring
// internal/modpackinstall.
package setupapply

import (
	"strings"

	"emperror.dev/errors"
	"github.com/google/uuid"
)

// Operator is one ops.json entry the panel asks for. Level is Minecraft's
// permission level, 1 to 4.
type Operator struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Level int    `json:"level"`
}

// WhitelistPlayer is one whitelist.json entry the panel asks for.
type WhitelistPlayer struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// Whitelist wraps the player list so "absent" (nil, write nothing) and
// "present but empty" (merge nothing, still a valid call) stay distinct.
type Whitelist struct {
	Players []WhitelistPlayer `json:"players"`
}

// Request is the wire payload of POST /api/servers/:server/setup-apply.
// Every field except SetupID is optional; a request carrying only SetupID
// is the Bedrock and Hytale case, which stops and starts the server.
type Request struct {
	SetupID    string            `json:"setup_id"`
	Eula       bool              `json:"eula"`
	Operators  []Operator        `json:"operators"`
	Whitelist  *Whitelist        `json:"whitelist"`
	Properties map[string]string `json:"properties"`
}

// Validate rejects anything the apply step is not written to handle.
// Unknown or malformed values are refused; there are no default behaviors.
func (r *Request) Validate() error {
	if _, err := uuid.Parse(r.SetupID); err != nil {
		return errors.New("setupapply: setup_id must be a UUID")
	}

	for _, operator := range r.Operators {
		if _, err := uuid.Parse(operator.UUID); err != nil {
			return errors.New("setupapply: operator uuid must be a UUID")
		}
		if operator.Name == "" {
			return errors.New("setupapply: operator name must not be empty")
		}
		if operator.Level < 1 || operator.Level > 4 {
			return errors.Errorf("setupapply: operator level %d is outside 1 to 4", operator.Level)
		}
	}

	if r.Whitelist != nil {
		for _, player := range r.Whitelist.Players {
			if _, err := uuid.Parse(player.UUID); err != nil {
				return errors.New("setupapply: whitelist uuid must be a UUID")
			}
			if player.Name == "" {
				return errors.New("setupapply: whitelist name must not be empty")
			}
		}
	}

	for key, value := range r.Properties {
		if key == "" || strings.ContainsAny(key, "=: \t\r\n") {
			return errors.Errorf("setupapply: property key %q is not a plain properties key", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return errors.Errorf("setupapply: property %q value must be a single line", key)
		}
	}

	return nil
}

// IsEmpty reports whether the request asks for no file changes at all, in
// which case the job only stops and starts the server.
func (r *Request) IsEmpty() bool {
	return !r.Eula && len(r.Operators) == 0 && r.Whitelist == nil && len(r.Properties) == 0
}
