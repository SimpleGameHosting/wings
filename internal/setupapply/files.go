package setupapply

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/server/filesystem"
)

// MaxSetupFileBytes bounds every read the apply step performs, so a
// corrupt or hostile game file cannot hold the operation reservation past
// the job timeout.
const MaxSetupFileBytes = 1 << 20

const (
	EulaFileName       = "eula.txt"
	OpsFileName        = "ops.json"
	WhitelistFileName  = "whitelist.json"
	PropertiesFileName = "server.properties"

	eulaContent = "eula=true\n"

	// tempSuffix names the staging file every write publishes from, so a
	// crash mid-write never leaves a truncated game file behind.
	tempSuffix = ".sgh-setup.tmp"
)

// Apply runs the file changes a request asks for in the spec's order:
// eula, operators, whitelist, properties. Absent fields write nothing.
func Apply(fs *filesystem.Filesystem, r Request) error {
	if r.Eula {
		if err := WriteEula(fs); err != nil {
			return err
		}
	}
	if len(r.Operators) > 0 {
		if err := MergeOperators(fs, r.Operators); err != nil {
			return err
		}
	}
	if r.Whitelist != nil {
		if err := MergeWhitelist(fs, r.Whitelist.Players); err != nil {
			return err
		}
	}
	if len(r.Properties) > 0 {
		if err := PatchProperties(fs, r.Properties); err != nil {
			return err
		}
	}
	return nil
}

// ReadBounded returns the content of a regular file at name, or existed
// false when nothing is there. A directory, fifo, or anything larger than
// MaxSetupFileBytes is an error rather than something to work around.
func ReadBounded(fs *filesystem.Filesystem, name string) ([]byte, bool, error) {
	info, err := fs.UnixFS().Lstat(name)
	if err != nil {
		if errors.Is(err, ufs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, errors.Wrap(err, "setupapply: failed to stat "+name)
	}
	if !info.Mode().IsRegular() {
		return nil, true, errors.Errorf("setupapply: %s is not a regular file", name)
	}
	if info.Size() > MaxSetupFileBytes {
		return nil, true, errors.Errorf("setupapply: %s is larger than the %d byte limit", name, MaxSetupFileBytes)
	}

	file, err := fs.UnixFS().Open(name)
	if err != nil {
		return nil, true, errors.Wrap(err, "setupapply: failed to open "+name)
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, MaxSetupFileBytes+1))
	if err != nil {
		return nil, true, errors.Wrap(err, "setupapply: failed to read "+name)
	}
	if len(content) > MaxSetupFileBytes {
		return nil, true, errors.Errorf("setupapply: %s grew past the %d byte limit while reading", name, MaxSetupFileBytes)
	}
	return content, true, nil
}

// WriteEula publishes the fixed eula.txt, overwriting whatever is there.
func WriteEula(fs *filesystem.Filesystem) error {
	return publish(fs, EulaFileName, []byte(eulaContent))
}

// MergeOperators merges the requested operators into ops.json by uuid:
// known uuids get their name and level updated and keep every other field,
// unknown uuids are appended with bypassesPlayerLimit false.
func MergeOperators(fs *filesystem.Filesystem, operators []Operator) error {
	entries, err := readPlayerList(fs, OpsFileName)
	if err != nil {
		return err
	}
	for _, operator := range operators {
		entry, found := findByUUID(entries, operator.UUID)
		if found {
			entry["name"] = operator.Name
			entry["level"] = operator.Level
			continue
		}
		entries = append(entries, map[string]any{
			"uuid":                operator.UUID,
			"name":                operator.Name,
			"level":               operator.Level,
			"bypassesPlayerLimit": false,
		})
	}
	return writePlayerList(fs, OpsFileName, entries)
}

// MergeWhitelist merges the requested players into whitelist.json by uuid,
// updating the name of a known uuid and appending unknown ones.
func MergeWhitelist(fs *filesystem.Filesystem, players []WhitelistPlayer) error {
	entries, err := readPlayerList(fs, WhitelistFileName)
	if err != nil {
		return err
	}
	for _, player := range players {
		entry, found := findByUUID(entries, player.UUID)
		if found {
			entry["name"] = player.Name
			continue
		}
		entries = append(entries, map[string]any{"uuid": player.UUID, "name": player.Name})
	}
	return writePlayerList(fs, WhitelistFileName, entries)
}

// PatchProperties replaces the value of every patched key that exists in
// server.properties, appends the rest sorted, and leaves every other line
// untouched, including comments, blank lines, and the file's line ending
// style. A missing file is created with only the patched keys. An empty
// patch does nothing at all.
func PatchProperties(fs *filesystem.Filesystem, patch map[string]string) error {
	if len(patch) == 0 {
		return nil
	}

	content, _, err := ReadBounded(fs, PropertiesFileName)
	if err != nil {
		return err
	}

	// The file's own convention wins for every line we emit; a file with
	// no line breaks at all is treated as LF, which is what Minecraft
	// itself writes...
	newline := "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		newline = "\r\n"
	}

	remaining := make(map[string]string, len(patch))
	for key, value := range patch {
		remaining[key] = value
	}

	// seen tracks every patched key that matched at least one line, so a
	// key repeated across duplicate lines (java.util.Properties is
	// last-wins) gets every occurrence rewritten instead of only the
	// first, and is not appended again at the end...
	seen := make(map[string]bool, len(patch))

	var out bytes.Buffer
	lines := splitKeepingTrailing(string(content))
	for _, line := range lines {
		key, isPair := propertyKey(line)
		if isPair {
			if value, patched := remaining[key]; patched {
				out.WriteString(key + "=" + value + newline)
				seen[key] = true
				continue
			}
		}
		out.WriteString(line)
	}

	// Anything not replaced in place is appended, sorted so the output is
	// deterministic, after making sure the last existing line was
	// terminated...
	if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
		out.WriteString(newline)
	}
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		if seen[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.WriteString(key + "=" + remaining[key] + newline)
	}

	return publish(fs, PropertiesFileName, out.Bytes())
}

// readPlayerList reads a JSON list of objects from name; a missing or
// blank file is an empty list, malformed JSON is an error.
func readPlayerList(fs *filesystem.Filesystem, name string) ([]map[string]any, error) {
	content, existed, err := ReadBounded(fs, name)
	if err != nil {
		return nil, err
	}
	if !existed || len(bytes.TrimSpace(content)) == 0 {
		return []map[string]any{}, nil
	}

	var entries []map[string]any
	if err := json.Unmarshal(content, &entries); err != nil {
		return nil, errors.Wrap(err, "setupapply: "+name+" is not a JSON list of players")
	}
	return entries, nil
}

// writePlayerList publishes entries to name pretty-printed with two space
// indentation, the layout the game itself writes.
func writePlayerList(fs *filesystem.Filesystem, name string, entries []map[string]any) error {
	content, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return errors.Wrap(err, "setupapply: failed to encode "+name)
	}
	return publish(fs, name, append(content, '\n'))
}

// findByUUID returns the entry whose uuid field equals id, if any.
func findByUUID(entries []map[string]any, id string) (map[string]any, bool) {
	for _, entry := range entries {
		if existing, ok := entry["uuid"].(string); ok && strings.EqualFold(existing, id) {
			return entry, true
		}
	}
	return nil, false
}

// propertyKey extracts the key of a key=value line, reporting false for
// comments, blank lines, and anything without a separator. Only "=" is
// honored as the separator because it is the only one the game writes.
func propertyKey(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || trimmed[0] == '#' || trimmed[0] == '!' {
		return "", false
	}
	index := strings.Index(trimmed, "=")
	if index < 0 {
		return "", false
	}
	return strings.TrimRight(trimmed[:index], " \t"), true
}

// splitKeepingTrailing splits content into lines that each keep their own
// line terminator, so re-emitting them reproduces the file byte for byte.
func splitKeepingTrailing(content string) []string {
	var lines []string
	for len(content) > 0 {
		index := strings.Index(content, "\n")
		if index < 0 {
			lines = append(lines, content)
			break
		}
		lines = append(lines, content[:index+1])
		content = content[index+1:]
	}
	return lines
}

// publish writes content to a sibling temp file and atomically replaces
// name with it, through the quota-accounted filesystem layer.
func publish(fs *filesystem.Filesystem, name string, content []byte) error {
	temp := name + tempSuffix
	if err := fs.Write(temp, bytes.NewReader(content), int64(len(content)), 0o644); err != nil {
		// Write touches and truncates the temp file before copying into
		// it, so a failed write can still leave a staged file behind and
		// charged to quota; clean it up on this path too...
		_ = fs.Delete(temp)
		return errors.Wrap(err, "setupapply: failed to stage "+name)
	}
	if err := fs.Replace(temp, name); err != nil {
		_ = fs.Delete(temp)
		return errors.Wrap(err, "setupapply: failed to publish "+name)
	}
	return nil
}
