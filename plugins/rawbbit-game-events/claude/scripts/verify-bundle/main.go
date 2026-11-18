// Command verify-bundle verifies the self-contained SDK bundle without access
// to its source repository. Run it from the plugin folder:
//
//	go run ./scripts/verify-bundle
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const manifestName = "BUNDLE-MANIFEST.json"

var (
	sdkNames = []string{"rawbbit-browser-game", "rawbbit-unity"}
	hashRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func main() {
	plugin := flag.String("plugin", ".", "plugin root folder")
	flag.Parse()
	root, err := filepath.Abs(*plugin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SDK bundle error: %v\n", err)
		os.Exit(1)
	}
	bundle := filepath.Join(root, "skills/rawbbit-game-events/assets/sdk")
	contract := filepath.Join(root, "skills/rawbbit-game-events/references/collector-contract.json")
	license := filepath.Join(root, "LICENSE")
	count, err := verify(bundle, contract, license)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SDK bundle error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Verified %d packaged SDK files\n", count)
}

func digest(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func isSDKName(name string) bool {
	for _, candidate := range sdkNames {
		if name == candidate {
			return true
		}
	}
	return false
}

func checkSafePath(raw string) error {
	unsafe := fmt.Errorf("Unsafe bundle path: %s", pyRepr(raw))
	if strings.ContainsAny(raw, "\\\x00") || strings.HasPrefix(raw, "/") || path.Clean(raw) != raw {
		return unsafe
	}
	parts := strings.Split(raw, "/")
	if len(parts) < 2 || !isSDKName(parts[0]) {
		return unsafe
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return unsafe
		}
	}
	return nil
}

func keySet(value any) (map[string]bool, bool) {
	keys := map[string]bool{}
	switch typed := value.(type) {
	case map[string]any:
		for key := range typed {
			keys[key] = true
		}
	case []any:
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return nil, false
			}
			keys[name] = true
		}
	default:
		return nil, false
	}
	return keys, true
}

func sameKeys(value any, want ...string) bool {
	keys, ok := keySet(value)
	if !ok || len(keys) != len(want) {
		return false
	}
	for _, key := range want {
		if !keys[key] {
			return false
		}
	}
	return true
}

func sortedDiff(left, right map[string]bool) []string {
	var out []string
	for key := range left {
		if !right[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "'" + item + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func verify(bundle, contract, license string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(bundle, manifestName))
	if err != nil {
		return 0, err
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, err
	}
	if !sameKeys(manifest, "format_version", "sdk_versions", "license", "collector_contract_sha256", "files") {
		return 0, errors.New("Unexpected bundle manifest fields")
	}
	if version, ok := manifest["format_version"].(float64); !ok || version != 1 || !sameKeys(manifest["sdk_versions"], sdkNames...) {
		return 0, errors.New("Invalid bundle manifest version or SDK names")
	}
	declaredRaw, ok := manifest["files"].(map[string]any)
	if !ok || len(declaredRaw) == 0 {
		return 0, errors.New("Empty or invalid bundle inventory")
	}
	declared := map[string]string{}
	declaredSet := map[string]bool{}
	for name, value := range declaredRaw {
		if err := checkSafePath(name); err != nil {
			return 0, err
		}
		hash, ok := value.(string)
		if !ok || !hashRE.MatchString(hash) {
			return 0, fmt.Errorf("Invalid bundle hash: %s", name)
		}
		declared[name] = hash
		declaredSet[name] = true
	}

	actual := map[string]bool{}
	err = filepath.WalkDir(bundle, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("Symlink in SDK bundle: %s", current)
		}
		if !entry.Type().IsRegular() || current == filepath.Join(bundle, manifestName) {
			return nil
		}
		relative, err := filepath.Rel(bundle, current)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(relative)] = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	missing, extra := sortedDiff(declaredSet, actual), sortedDiff(actual, declaredSet)
	if len(missing) > 0 || len(extra) > 0 {
		return 0, fmt.Errorf("SDK inventory mismatch: missing=%s, extra=%s", pyList(missing), pyList(extra))
	}
	for name, expected := range declared {
		got, err := digest(filepath.Join(bundle, filepath.FromSlash(name)))
		if err != nil {
			return 0, err
		}
		if got != expected {
			return 0, fmt.Errorf("SDK hash mismatch: %s", name)
		}
	}

	licenseInfo, ok := manifest["license"].(map[string]any)
	if !ok || !sameKeys(licenseInfo, "id", "sha256") || licenseInfo["id"] != "Apache-2.0" {
		return 0, errors.New("Invalid bundle license metadata")
	}
	licenseHash, err := digest(license)
	if err != nil {
		return 0, err
	}
	if licenseHash != licenseInfo["sha256"] {
		return 0, errors.New("Plugin license hash mismatch")
	}
	for _, name := range sdkNames {
		sdkLicense := name + "/LICENSE"
		if _, ok := declared[sdkLicense]; !ok {
			return 0, fmt.Errorf("Missing or mismatched SDK license: %s", name)
		}
		got, err := digest(filepath.Join(bundle, name, "LICENSE"))
		if err != nil {
			return 0, err
		}
		if got != licenseInfo["sha256"] {
			return 0, fmt.Errorf("Missing or mismatched SDK license: %s", name)
		}
	}
	contractHash, err := digest(contract)
	if err != nil {
		return 0, err
	}
	if contractHash != manifest["collector_contract_sha256"] {
		return 0, errors.New("Collector contract facts hash mismatch")
	}
	return len(declared), nil
}

// pyRepr quotes a string the way the original Python tool reported paths.
func pyRepr(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`, "\x00", `\x00`).Replace(value) + "'"
}
