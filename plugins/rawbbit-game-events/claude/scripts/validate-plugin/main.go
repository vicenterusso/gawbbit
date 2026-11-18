// Command validate-plugin validates the self-contained Claude plugin without
// private source access. Run it from the plugin folder:
//
//	go run ./scripts/validate-plugin
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mirlan-irokez/rawbbit/plugins/rawbbit-game-events/claude/scripts/internal/plugincheck"
)

var (
	versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	fieldRE   = regexp.MustCompile(`(?m)^(name|description):\s*(.+)$`)
	required  = []string{
		"LICENSE",
		"rawbbit-icon.svg",
		"scripts/verify-bundle/main.go",
		"skills/rawbbit-game-events/assets/sdk/BUNDLE-MANIFEST.json",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-unity/LICENSE",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-unity/README.md",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-unity/INTEGRATION.md",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-unity/Examples/GameAnalyticsTemplate/GameAnalyticsAdapter.cs.template",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-browser-game/LICENSE",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-browser-game/README.md",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-browser-game/INTEGRATION.md",
		"skills/rawbbit-game-events/assets/sdk/rawbbit-browser-game/examples/GameAnalyticsAdapter.js.template",
		"skills/rawbbit-game-events/references/collector-contract.json",
	}
	requiredFields = []string{"name", "description", "version", "author", "license"}
	linkFields     = []string{"homepage", "repository", "privacyPolicyUrl", "supportUrl"}
)

func main() {
	dir := flag.String("plugin", ".", "plugin root folder")
	flag.Parse()
	count, err := validate(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Claude plugin validation error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Validated Claude manifest, skill, required assets, and links in %d documents\n", count)
}

func readJSON(name string) (map[string]any, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func validManifestKeys(manifest map[string]any) bool {
	allowed := map[string]bool{"displayName": true, "keywords": true}
	for _, key := range append(requiredFields, linkFields...) {
		allowed[key] = true
	}
	for _, key := range requiredFields {
		if _, ok := manifest[key]; !ok {
			return false
		}
	}
	for key := range manifest {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func validateManifest(plugin string, manifest map[string]any) error {
	if !validManifestKeys(manifest) {
		return errors.New("Unexpected Claude manifest fields")
	}
	for _, key := range append([]string{"displayName"}, linkFields...) {
		if value, ok := manifest[key]; ok && !plugincheck.NonBlank(value) {
			return fmt.Errorf("Invalid Claude manifest field: %s", key)
		}
	}
	for _, key := range linkFields {
		if value, ok := manifest[key]; ok {
			parsed, err := url.Parse(value.(string))
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
				return fmt.Errorf("Claude manifest %s must be an https URL", key)
			}
		}
	}
	if keywords, ok := manifest["keywords"]; ok {
		items, isList := keywords.([]any)
		if !isList {
			return errors.New("Invalid Claude manifest keywords")
		}
		for _, item := range items {
			if !plugincheck.NonBlank(item) {
				return errors.New("Invalid Claude manifest keywords")
			}
		}
	}
	if manifest["name"] != "rawbbit-game-events" {
		return errors.New("Unexpected plugin name")
	}
	for _, key := range []string{"description", "version", "license"} {
		if !plugincheck.NonBlank(manifest[key]) {
			return errors.New("Missing Claude plugin metadata")
		}
	}
	if !versionRE.MatchString(manifest["version"].(string)) {
		return errors.New("Invalid plugin version")
	}
	author, ok := manifest["author"].(map[string]any)
	if !ok || !plugincheck.NonBlank(author["name"]) {
		return errors.New("Missing plugin author")
	}
	if manifest["license"] != "Apache-2.0" {
		return errors.New("Unexpected license")
	}
	for _, forbidden := range []string{".mcp.json", "mcp.json"} {
		if _, err := os.Lstat(filepath.Join(plugin, forbidden)); err == nil {
			return errors.New("Skills-only package contains MCP configuration")
		}
	}
	readme, err := os.ReadFile(filepath.Join(plugin, "README.md"))
	if err != nil {
		return err
	}
	if len(strings.Fields(string(readme))) < 40 {
		return errors.New("Plugin README is too short")
	}
	return nil
}

func validateSkill(skill string, name any) error {
	raw, err := os.ReadFile(skill)
	if err != nil {
		return err
	}
	parts := plugincheck.SkillFrontmatter.FindStringSubmatch(string(raw))
	if parts == nil || strings.TrimSpace(parts[2]) == "" {
		return errors.New("Missing skill frontmatter or body")
	}
	fields := map[string]string{}
	for _, match := range fieldRE.FindAllStringSubmatch(parts[1], -1) {
		fields[match[1]] = match[2]
	}
	if fields["name"] != name || strings.TrimSpace(fields["description"]) == "" {
		return errors.New("Invalid skill metadata")
	}
	if !strings.Contains(string(raw), "## Claude execution surface") {
		return errors.New("Missing Claude local-execution boundary")
	}
	return nil
}

func validate(dir string) (int, error) {
	plugin, err := plugincheck.PluginRoot(dir)
	if err != nil {
		return 0, err
	}
	skillRoot := filepath.Join(plugin, "skills/rawbbit-game-events")
	manifest, err := readJSON(filepath.Join(plugin, ".claude-plugin/plugin.json"))
	if err != nil {
		return 0, err
	}
	if err := validateManifest(plugin, manifest); err != nil {
		return 0, err
	}
	skill := filepath.Join(skillRoot, "SKILL.md")
	if err := validateSkill(skill, manifest["name"]); err != nil {
		return 0, err
	}
	for _, relative := range required {
		info, err := os.Lstat(filepath.Join(plugin, relative))
		if err != nil || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("Missing or unsafe package file: %s", relative)
		}
	}
	contract, err := readJSON(filepath.Join(skillRoot, "references/collector-contract.json"))
	if err != nil {
		return 0, err
	}
	models, _ := contract["models"].(map[string]any)
	if !plugincheck.Truthy(models["EventEnvelope"]) || !plugincheck.Truthy(contract["defaults"]) {
		return 0, errors.New("Missing Collector contract facts")
	}
	documents, err := plugincheck.Documents(plugin, skill)
	if err != nil {
		return 0, err
	}
	if err := plugincheck.CheckLinks(plugin, documents, "Broken local link"); err != nil {
		return 0, err
	}
	return len(documents), nil
}
