package setupapply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteEulaOverwrites(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, EulaFileName, "eula=false\n")
	if err := WriteEula(fs); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, fs, EulaFileName); got != "eula=true\n" {
		t.Fatalf("eula.txt = %q", got)
	}
}

// TestMergeOperatorsPreservesExistingEntriesAndFields proves the merge is
// keyed on uuid, keeps unknown fields on entries it updates, updates name
// and level for a known uuid, and appends unknown uuids with the fixed
// bypassesPlayerLimit false.
func TestMergeOperatorsPreservesExistingEntriesAndFields(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, OpsFileName, `[{"uuid":"11111111-1111-1111-1111-111111111111","name":"Old","level":2,"bypassesPlayerLimit":true,"custom":"kept"},{"uuid":"22222222-2222-2222-2222-222222222222","name":"Other","level":4,"bypassesPlayerLimit":false}]`)

	err := MergeOperators(fs, []Operator{
		{UUID: "11111111-1111-1111-1111-111111111111", Name: "New", Level: 4},
		{UUID: "33333333-3333-3333-3333-333333333333", Name: "Added", Level: 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	var entries []map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, fs, OpsFileName)), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0]["name"] != "New" || entries[0]["level"] != float64(4) || entries[0]["custom"] != "kept" || entries[0]["bypassesPlayerLimit"] != true {
		t.Fatalf("updated entry lost fields: %v", entries[0])
	}
	if entries[1]["name"] != "Other" {
		t.Fatalf("untouched entry changed: %v", entries[1])
	}
	if entries[2]["uuid"] != "33333333-3333-3333-3333-333333333333" || entries[2]["bypassesPlayerLimit"] != false || entries[2]["level"] != float64(3) {
		t.Fatalf("appended entry wrong: %v", entries[2])
	}
}

func TestMergeOperatorsCreatesFileWhenMissing(t *testing.T) {
	fs := newTestFs(t)
	if err := MergeOperators(fs, []Operator{{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane", Level: 4}}); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, fs, OpsFileName)
	if !strings.Contains(got, `"uuid": "11111111-1111-1111-1111-111111111111"`) {
		t.Fatalf("ops.json = %q", got)
	}
}

func TestMergeOperatorsTreatsEmptyFileAsEmptyList(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, OpsFileName, "")
	if err := MergeOperators(fs, []Operator{{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane", Level: 4}}); err != nil {
		t.Fatal(err)
	}
}

func TestMergeOperatorsRejectsMalformedJSON(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, OpsFileName, "{not json")
	if err := MergeOperators(fs, []Operator{{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane", Level: 4}}); err == nil {
		t.Fatal("expected malformed ops.json to be rejected")
	}
}

func TestMergeWhitelistAppendsAndDeduplicates(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, WhitelistFileName, `[{"uuid":"11111111-1111-1111-1111-111111111111","name":"Kane"}]`)
	err := MergeWhitelist(fs, []WhitelistPlayer{
		{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane"},
		{UUID: "22222222-2222-2222-2222-222222222222", Name: "Alex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, fs, WhitelistFileName)), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(entries), entries)
	}
}

// TestPatchPropertiesPreservesEverythingElse is the heart of the patch:
// comments, blank lines, unknown keys, and key order survive; patched keys
// are replaced in place; new keys are appended; CRLF endings are kept.
func TestPatchPropertiesPreservesEverythingElse(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, PropertiesFileName, "#Minecraft server properties\r\n#Tue Sep 02\r\nmotd=Hello\r\nwhite-list=false\r\n\r\nmax-players=20\r\n")

	if err := PatchProperties(fs, map[string]string{"white-list": "true", "enable-query": "true"}); err != nil {
		t.Fatal(err)
	}

	got := mustRead(t, fs, PropertiesFileName)
	want := "#Minecraft server properties\r\n#Tue Sep 02\r\nmotd=Hello\r\nwhite-list=true\r\n\r\nmax-players=20\r\nenable-query=true\r\n"
	if got != want {
		t.Fatalf("server.properties =\n%q\nwant\n%q", got, want)
	}
}

// TestPatchPropertiesRewritesEveryDuplicateOccurrence proves a patched key
// that appears more than once in the file gets every occurrence rewritten,
// matching java.util.Properties' last-wins semantics, rather than only the
// first line encountered.
func TestPatchPropertiesRewritesEveryDuplicateOccurrence(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, PropertiesFileName, "white-list=false\nmotd=Hello\nwhite-list=false\n")
	if err := PatchProperties(fs, map[string]string{"white-list": "true"}); err != nil {
		t.Fatal(err)
	}
	want := "white-list=true\nmotd=Hello\nwhite-list=true\n"
	if got := mustRead(t, fs, PropertiesFileName); got != want {
		t.Fatalf("server.properties = %q, want %q", got, want)
	}
}

func TestPatchPropertiesUsesLFWhenFileUsesLF(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, PropertiesFileName, "motd=Hello\nwhite-list=false\n")
	if err := PatchProperties(fs, map[string]string{"white-list": "true", "query.port": "25565"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, fs, PropertiesFileName); got != "motd=Hello\nwhite-list=true\nquery.port=25565\n" {
		t.Fatalf("server.properties = %q", got)
	}
}

func TestPatchPropertiesAppendsNewlineBeforeAppendingWhenMissing(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, PropertiesFileName, "motd=Hello")
	if err := PatchProperties(fs, map[string]string{"enable-query": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, fs, PropertiesFileName); got != "motd=Hello\nenable-query=true\n" {
		t.Fatalf("server.properties = %q", got)
	}
}

func TestPatchPropertiesCreatesMinimalFileWhenMissing(t *testing.T) {
	fs := newTestFs(t)
	if err := PatchProperties(fs, map[string]string{"white-list": "true", "enable-query": "true"}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, fs, PropertiesFileName); got != "enable-query=true\nwhite-list=true\n" {
		t.Fatalf("new keys must be written sorted, got %q", got)
	}
}

func TestPatchPropertiesWithEmptyPatchTouchesNothing(t *testing.T) {
	fs := newTestFs(t)
	if err := PatchProperties(fs, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.UnixFS().Lstat(PropertiesFileName); err == nil {
		t.Fatal("empty patch must not create server.properties")
	}
}

func TestReadBoundedRefusesOversizedAndNonRegularFiles(t *testing.T) {
	fs := newTestFs(t)
	mustWrite(t, fs, OpsFileName, strings.Repeat("x", MaxSetupFileBytes+1))
	if _, _, err := ReadBounded(fs, OpsFileName); err == nil {
		t.Fatal("expected an oversized file to be refused")
	}

	root := fs.Path()
	if err := syscall.Mkfifo(filepath.Join(root, WhitelistFileName), 0o644); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	if _, _, err := ReadBounded(fs, WhitelistFileName); err == nil {
		t.Fatal("expected a fifo to be refused")
	}

	_, existed, err := ReadBounded(fs, PropertiesFileName)
	if err != nil || existed {
		t.Fatalf("missing file must read as absent without error, got existed=%v err=%v", existed, err)
	}
	_ = os.Remove(filepath.Join(root, WhitelistFileName))
}

// TestApplyRunsEverythingInOrderAndSkipsAbsentFields runs a full request
// and an empty one through Apply.
func TestApplyRunsEverythingInOrderAndSkipsAbsentFields(t *testing.T) {
	fs := newTestFs(t)
	full := Request{
		Eula:       true,
		Operators:  []Operator{{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane", Level: 4}},
		Whitelist:  &Whitelist{Players: []WhitelistPlayer{{UUID: "11111111-1111-1111-1111-111111111111", Name: "Kane"}}},
		Properties: map[string]string{"white-list": "true"},
	}
	if err := Apply(fs, full); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EulaFileName, OpsFileName, WhitelistFileName, PropertiesFileName} {
		if _, err := fs.UnixFS().Lstat(name); err != nil {
			t.Fatalf("%s missing after full apply: %v", name, err)
		}
	}

	empty := newTestFs(t)
	if err := Apply(empty, Request{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EulaFileName, OpsFileName, WhitelistFileName, PropertiesFileName} {
		if _, err := empty.UnixFS().Lstat(name); err == nil {
			t.Fatalf("%s must not exist after an empty apply", name)
		}
	}
}

// TestPublishLeavesNoStagingFileBehind proves a successful publish cleans
// up its own staging file rather than leaving a .sgh-setup.tmp entry
// charged to quota on the server root.
func TestPublishLeavesNoStagingFileBehind(t *testing.T) {
	fs := newTestFs(t)
	if err := PatchProperties(fs, map[string]string{"white-list": "true"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fs.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), tempSuffix) {
			t.Fatalf("staging file left behind after publish: %s", entry.Name())
		}
	}
}
