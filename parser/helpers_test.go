package parser

import (
	"testing"

	"github.com/buger/jsonparser"
)

// regexReplacement builds a replacement that rewrites match only when the
// existing value satisfies the regular expression in if_value.
func regexReplacement(match, expression, replaceWith string) ConfigurationFileReplacement {
	return ConfigurationFileReplacement{
		Match:       match,
		IfValue:     "regex:" + expression,
		ReplaceWith: ReplaceValue{value: []byte(replaceWith), valueType: jsonparser.String},
	}
}

// TestIterateOverJsonAppliesARegexReplacementToAnExistingValue is the case
// the regex form of if_value exists for: the key is present and matches, so
// the matched part is rewritten and the rest of the value is kept.
func TestIterateOverJsonAppliesARegexReplacementToAnExistingValue(t *testing.T) {
	f := &ConfigurationFile{Replace: []ConfigurationFileReplacement{regexReplacement("motd", "^old", "new")}}

	parsed, err := f.IterateOverJson([]byte(`{"motd":"old server","other":"untouched"}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := parsed.Path("motd").Data(); got != "new server" {
		t.Fatalf("expected the regex replacement to apply, got %v", got)
	}
	if got := parsed.Path("other").Data(); got != "untouched" {
		t.Fatalf("expected unrelated keys to be left alone, got %v", got)
	}
}

// TestIterateOverJsonLeavesANonMatchingValueAlone keeps a value that does
// not satisfy the expression exactly as it was.
func TestIterateOverJsonLeavesANonMatchingValueAlone(t *testing.T) {
	f := &ConfigurationFile{Replace: []ConfigurationFileReplacement{regexReplacement("motd", "^old", "new")}}

	parsed, err := f.IterateOverJson([]byte(`{"motd":"current server"}`))
	if err != nil {
		t.Fatal(err)
	}

	if got := parsed.Path("motd").Data(); got != "current server" {
		t.Fatalf("expected the value to be untouched, got %v", got)
	}
}

// TestIterateOverJsonSkipsARegexReplacementForAMissingKey never invents a
// key for a regex replacement, since there is no existing value to rewrite.
func TestIterateOverJsonSkipsARegexReplacementForAMissingKey(t *testing.T) {
	f := &ConfigurationFile{Replace: []ConfigurationFileReplacement{regexReplacement("motd", "^old", "new")}}

	parsed, err := f.IterateOverJson([]byte(`{"other":"untouched"}`))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.ExistsP("motd") {
		t.Fatalf("expected no key to be created, got %s", parsed.String())
	}
}
