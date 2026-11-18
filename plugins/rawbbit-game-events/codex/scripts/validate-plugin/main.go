// Command validate-plugin validates a clean extracted Codex plugin. Run it
// from the plugin folder:
//
//	go run ./scripts/validate-plugin
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/cases"
	"gopkg.in/yaml.v3"

	"github.com/mirlan-irokez/rawbbit/plugins/rawbbit-game-events/codex/scripts/internal/plugincheck"
)

var required = []string{
	"LICENSE",
	"assets/rawbbit-icon.svg",
	"skills/rawbbit-game-events/assets/sdk/BUNDLE-MANIFEST.json",
	"scripts/verify-bundle/main.go",
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

func main() {
	dir := flag.String("plugin", ".", "plugin root folder")
	flag.Parse()
	count, err := validate(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Plugin validation error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Validated plugin manifest, listing, skill, required assets, and links in %d documents\n", count)
}

func readJSON(name string) (any, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// compileSchema checks the schema against the 2020-12 metaschema and compiles it.
func compileSchema(name string) (*jsonschema.Schema, error) {
	doc, err := readJSON(name)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseRegexpEngine(compileLookaround)
	location := "file://" + filepath.ToSlash(name)
	if err := compiler.AddResource(location, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

func object(value any) map[string]any {
	typed, _ := value.(map[string]any)
	return typed
}

func validString(value any, limit int, singleLine bool) bool {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > limit {
		return false
	}
	return !singleLine || !strings.ContainsAny(text, "\r\n")
}

func validateStrings(value any, minItems, maxItems, limit int) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) < minItems || len(items) > maxItems {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !validString(item, limit, false) {
			return nil, false
		}
		out = append(out, item.(string))
	}
	return out, true
}

func validateListing(plugin string, manifest map[string]any) error {
	extension := object(object(manifest["extensions"])["com.openai"])
	if extension["requires_local_executor"] != true {
		return errors.New("Local game checkout requires desktop execution")
	}
	interfaceValue, ok := extension["interface"].(map[string]any)
	if !ok {
		return errors.New("Missing OpenAI listing interface")
	}
	for _, field := range []struct {
		name  string
		limit int
	}{{"displayName", 30}, {"shortDescription", 30}, {"longDescription", 4000}, {"developerName", 80}} {
		value, ok := interfaceValue[field.name].(string)
		if !ok || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > field.limit {
			return fmt.Errorf("Invalid listing field: %s", field.name)
		}
		if field.name != "longDescription" && strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("Listing field must be single-line: %s", field.name)
		}
	}
	if !plugincheck.NonBlank(interfaceValue["category"]) {
		return errors.New("Missing listing category")
	}
	if _, ok := validateStrings(interfaceValue["capabilities"], 1, 20, 120); !ok {
		return errors.New("Invalid listing capabilities")
	}
	prompts := interfaceValue["defaultPrompt"]
	if prompts == nil {
		if _, present := interfaceValue["defaultPrompt"]; !present {
			prompts = []any{}
		}
	}
	items, ok := validateStrings(prompts, 0, 3, 128)
	if !ok || !unique(items) {
		return errors.New("Invalid or duplicate starter prompts")
	}
	for _, field := range []string{"logo", "composerIcon"} {
		if err := validateIcon(plugin, field, interfaceValue[field]); err != nil {
			return err
		}
	}
	return nil
}

func unique(items []string) bool {
	fold := cases.Fold()
	seen := map[string]bool{}
	for _, item := range items {
		seen[strings.TrimSpace(fold.String(item))] = true
	}
	return len(seen) == len(items)
}

func validateIcon(plugin, field string, value any) error {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, "./") {
		return fmt.Errorf("Missing or invalid listing asset: %s", field)
	}
	asset, inside := plugincheck.Within(plugin, filepath.Join(plugin, text))
	info, err := os.Stat(asset)
	if !inside || err != nil || !info.Mode().IsRegular() || strings.ToLower(filepath.Ext(asset)) != ".svg" {
		return fmt.Errorf("Missing or unsafe listing asset: %s", field)
	}
	if info.Size() > 5*1024*1024 {
		return fmt.Errorf("Listing asset too large: %s", field)
	}
	width, height, err := svgSize(asset)
	if err != nil {
		return err
	}
	if !width.ok || !height.ok {
		return fmt.Errorf("Listing icon must have numeric dimensions: %s", field)
	}
	if width.value != height.value || width.value < 48 {
		return fmt.Errorf("Listing icon must be square and at least 48px: %s", field)
	}
	return nil
}

type dimension struct {
	value float64
	ok    bool
}

// svgSize parses the whole document and returns the root width and height.
func svgSize(name string) (dimension, dimension, error) {
	file, err := os.Open(name)
	if err != nil {
		return dimension{}, dimension{}, err
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	var width, height dimension
	seenRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return dimension{}, dimension{}, err
		}
		start, isStart := token.(xml.StartElement)
		if !isStart || seenRoot {
			continue
		}
		seenRoot = true
		for _, attr := range start.Attr {
			if attr.Name.Space != "" {
				continue
			}
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(attr.Value), 64)
			switch attr.Name.Local {
			case "width":
				width = dimension{parsed, parseErr == nil}
			case "height":
				height = dimension{parsed, parseErr == nil}
			}
		}
	}
	if !seenRoot {
		return dimension{}, dimension{}, errors.New("no element found")
	}
	return width, height, nil
}

func validateMCP(plugin string) error {
	schema, err := compileSchema(filepath.Join(plugin, "scripts", "mcp.schema.json"))
	if err != nil {
		return err
	}
	config, err := readJSON(filepath.Join(plugin, "mcp.json"))
	if err != nil {
		return err
	}
	// Agent Plugins 1.0 MCP schema omits host-specific extensions that OpenAI's
	// plugin docs explicitly place on each server. Validate the portable core
	// against the schema, then validate the complete OpenAI overlay below.
	servers := object(object(config)["mcpServers"])
	portableServers := map[string]any{}
	for name, entry := range servers {
		if fields, ok := entry.(map[string]any); ok {
			copied := map[string]any{}
			for key, value := range fields {
				if key != "extensions" {
					copied[key] = value
				}
			}
			entry = copied
		}
		portableServers[name] = entry
	}
	portable := map[string]any{}
	for key, value := range object(config) {
		portable[key] = value
	}
	if servers != nil {
		portable["mcpServers"] = portableServers
	}
	if err := schema.Validate(portable); err != nil {
		return err
	}
	if len(servers) != 1 || servers["rawbbit-sdk-catalog"] == nil {
		return errors.New("Expected exactly the public Rawbbit SDK catalog MCP connection")
	}
	server := object(servers["rawbbit-sdk-catalog"])
	_, hasType := server["type"]
	_, hasURL := server["url"]
	_, hasExtensions := server["extensions"]
	if len(server) != 3 || !hasType || !hasURL || !hasExtensions {
		return errors.New("Unexpected SDK catalog MCP connection fields")
	}
	if server["type"] != "streamable-http" || server["url"] != "https://sdk-catalog.rawbbit.one/mcp" {
		return errors.New("Unexpected SDK catalog transport or endpoint")
	}
	auth := object(object(object(server["extensions"])["com.openai"])["auth"])
	if len(auth) != 1 || auth["type"] != "none" {
		return errors.New("Public SDK catalog must declare no authentication")
	}
	if plugincheck.Truthy(server["headers"]) || plugincheck.Truthy(server["env"]) {
		return errors.New("Public SDK catalog connection must not include credentials")
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
	var metadata any
	if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil {
		return err
	}
	fields, ok := metadata.(map[string]any)
	if !ok {
		return errors.New("Skill frontmatter must be a mapping")
	}
	if fields["name"] != name {
		return errors.New("Skill name does not match plugin")
	}
	if !validString(fields["description"], 1024, false) {
		return errors.New("Skill description is missing or invalid")
	}
	return nil
}

func validate(dir string) (int, error) {
	plugin, err := plugincheck.PluginRoot(dir)
	if err != nil {
		return 0, err
	}
	skill := filepath.Join(plugin, "skills", "rawbbit-game-events", "SKILL.md")
	skillRoot := filepath.Dir(skill)

	schema, err := compileSchema(filepath.Join(plugin, "scripts", "plugin.schema.json"))
	if err != nil {
		return 0, err
	}
	manifestValue, err := readJSON(filepath.Join(plugin, "plugin.json"))
	if err != nil {
		return 0, err
	}
	if err := schema.Validate(manifestValue); err != nil {
		return 0, err
	}
	manifest := object(manifestValue)
	if manifest["name"] != "rawbbit-game-events" {
		return 0, errors.New("Unexpected plugin name")
	}
	if err := validateListing(plugin, manifest); err != nil {
		return 0, err
	}
	if err := validateMCP(plugin); err != nil {
		return 0, err
	}
	if err := validateSkill(skill, manifest["name"]); err != nil {
		return 0, err
	}
	for _, relative := range required {
		info, err := os.Stat(filepath.Join(plugin, relative))
		if err != nil || !info.Mode().IsRegular() {
			return 0, fmt.Errorf("Missing required package file: %s", relative)
		}
	}
	contractValue, err := readJSON(filepath.Join(skillRoot, "references", "collector-contract.json"))
	if err != nil {
		return 0, err
	}
	contract := object(contractValue)
	if !plugincheck.Truthy(object(contract["models"])["EventEnvelope"]) || !plugincheck.Truthy(contract["defaults"]) {
		return 0, errors.New("Missing generated Collector contract facts")
	}
	documents, err := plugincheck.Documents(plugin, skill)
	if err != nil {
		return 0, err
	}
	if err := plugincheck.CheckLinks(plugin, documents, "Broken local documentation link"); err != nil {
		return 0, err
	}
	return len(documents), nil
}

// lookaroundRegexp lets schema patterns use lookahead, as the Python validator allowed.
type lookaroundRegexp regexp2.Regexp

func (re *lookaroundRegexp) MatchString(text string) bool {
	matched, err := (*regexp2.Regexp)(re).MatchString(text)
	return err == nil && matched
}

func (re *lookaroundRegexp) String() string { return (*regexp2.Regexp)(re).String() }

func compileLookaround(pattern string) (jsonschema.Regexp, error) {
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, err
	}
	return (*lookaroundRegexp)(re), nil
}
