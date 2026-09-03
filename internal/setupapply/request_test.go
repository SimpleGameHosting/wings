package setupapply

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validRequest() Request {
	return Request{
		SetupID:    uuid.NewString(),
		Eula:       true,
		Operators:  []Operator{{UUID: uuid.NewString(), Name: "Kane", Level: 4}},
		Whitelist:  &Whitelist{Players: []WhitelistPlayer{{UUID: uuid.NewString(), Name: "Alex"}}},
		Properties: map[string]string{"white-list": "true", "query.port": "25565"},
	}
}

// TestValidateAcceptsFullAndEmptyRequests covers the two shapes the panel
// sends: every field for a Java launch, and only setup_id for Bedrock.
func TestValidateAcceptsFullAndEmptyRequests(t *testing.T) {
	full := validRequest()
	if err := full.Validate(); err != nil {
		t.Fatalf("full request rejected: %v", err)
	}
	if full.IsEmpty() {
		t.Fatal("full request reported empty")
	}

	empty := Request{SetupID: uuid.NewString()}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty request rejected: %v", err)
	}
	if !empty.IsEmpty() {
		t.Fatal("bare setup_id request not reported empty")
	}
}

// TestValidateRejectsMalformedFields locks down every refusal: a bad setup
// id, a bad player uuid, an out of range level, and property keys or values
// the line-based patch could not represent.
func TestValidateRejectsMalformedFields(t *testing.T) {
	cases := map[string]func(r *Request){
		"setup_id not a uuid":    func(r *Request) { r.SetupID = "nope" },
		"operator uuid":          func(r *Request) { r.Operators[0].UUID = "nope" },
		"operator name empty":    func(r *Request) { r.Operators[0].Name = "" },
		"operator level zero":    func(r *Request) { r.Operators[0].Level = 0 },
		"operator level five":    func(r *Request) { r.Operators[0].Level = 5 },
		"whitelist uuid":         func(r *Request) { r.Whitelist.Players[0].UUID = "nope" },
		"whitelist name empty":   func(r *Request) { r.Whitelist.Players[0].Name = "" },
		"property key empty":     func(r *Request) { r.Properties[""] = "x" },
		"property key equals":    func(r *Request) { r.Properties["a=b"] = "x" },
		"property key colon":     func(r *Request) { r.Properties["a:b"] = "x" },
		"property key newline":   func(r *Request) { r.Properties["a\nb"] = "x" },
		"property key space":     func(r *Request) { r.Properties["a b"] = "x" },
		"property value newline": func(r *Request) { r.Properties["motd"] = "a\nb" },
		"property value cr":      func(r *Request) { r.Properties["motd"] = "a\rb" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			} else if !strings.HasPrefix(err.Error(), "setupapply: ") {
				t.Fatalf("error must carry the package prefix, got %q", err.Error())
			}
		})
	}
}

// TestValidateAcceptsEmptyWhitelist proves an explicit whitelist with no
// players is a valid no-op merge rather than an error.
func TestValidateAcceptsEmptyWhitelist(t *testing.T) {
	r := Request{SetupID: uuid.NewString(), Whitelist: &Whitelist{}}
	if err := r.Validate(); err != nil {
		t.Fatalf("empty whitelist rejected: %v", err)
	}
	if r.IsEmpty() {
		t.Fatal("a present whitelist must not count as empty")
	}
}
