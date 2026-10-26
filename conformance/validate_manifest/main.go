// Command validate_manifest validates a plugin manifest (as JSON) against the
// strict PluginBridge manifest JSON Schema
// (../prism-plugin-sdk/conformance/manifest.schema.json).
//
// It is a small generic interpreter for the JSON Schema draft 2020-12 subset
// that schema actually uses ($ref to #/$defs, type, const, enum, pattern,
// properties, required, additionalProperties, items, minItems, minLength,
// maxLength, uniqueItems). It interprets the schema document itself — nothing
// about the manifest fields is hardcoded here — so a schema update is picked up
// automatically.
//
// The repo manifest is YAML; conformance/run.sh converts YAML to JSON first
// (python3+PyYAML when available, otherwise macOS ruby stdlib) and pipes it in.
// Extra top-level properties the Hub understands but the SDK schema does not
// list yet (see run.sh) can be tolerated explicitly with -allow-extra; they are
// then reported as warnings instead of violations.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type violation struct {
	path    string
	message string
}

func main() {
	schemaPath := flag.String("schema", "../prism-plugin-sdk/conformance/manifest.schema.json", "path to manifest.schema.json")
	manifestPath := flag.String("manifest", "", "path to the manifest as JSON (required; use - for stdin)")
	allowExtra := flag.String("allow-extra", "", "comma-separated top-level properties tolerated beyond the schema (reported as warnings)")
	flag.Parse()

	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "validate_manifest: -manifest is required (JSON; YAML must be converted first)")
		os.Exit(2)
	}

	schemaBytes, err := os.ReadFile(*schemaPath)
	if err != nil {
		fatalf("read schema %s: %v", *schemaPath, err)
	}
	var schema any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		fatalf("parse schema %s: %v", *schemaPath, err)
	}
	schemaDoc, ok := schema.(map[string]any)
	if !ok {
		fatalf("schema %s: root is not an object", *schemaPath)
	}

	var manifestBytes []byte
	if *manifestPath == "-" {
		manifestBytes, err = os.ReadFile("/dev/stdin")
	} else {
		manifestBytes, err = os.ReadFile(*manifestPath)
	}
	if err != nil {
		fatalf("read manifest %s: %v", *manifestPath, err)
	}
	var manifest any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		fatalf("parse manifest %s: %v", *manifestPath, err)
	}

	allowed := map[string]bool{}
	for _, name := range strings.Split(*allowExtra, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = true
		}
	}

	vs := validate(manifest, schemaDoc, schemaDoc, "$", allowed)
	var warnings []violation
	var errors []violation
	for _, v := range vs {
		if v.path == "$" && strings.HasPrefix(v.message, "tolerated extra property") {
			warnings = append(warnings, v)
			continue
		}
		errors = append(errors, v)
	}

	name := *manifestPath
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "WARN  %s: %s\n", w.path, w.message)
	}
	if len(errors) > 0 {
		for _, e := range errors {
			fmt.Fprintf(os.Stderr, "FAIL  %s: %s\n", e.path, e.message)
		}
		fmt.Fprintf(os.Stderr, "validate_manifest: %s: %d violation(s) against %s\n", name, len(errors), *schemaPath)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "validate_manifest: %s: OK against %s (subset-checked keywords: type, const, enum, pattern, properties, required, additionalProperties, items, minItems, minLength, maxLength, uniqueItems, $ref)\n", name, *schemaPath)
}

// validate checks node against the subschema sch. root is the whole schema
// document for $ref resolution. path is a JSONPath-ish locator ("$.a.b[0]").
func validate(node any, sch, root map[string]any, path string, allowedExtra map[string]bool) []violation {
	if ref, ok := sch["$ref"].(string); ok {
		target, err := resolveRef(root, ref)
		if err != nil {
			return []violation{{path, fmt.Sprintf("schema error: %v", err)}}
		}
		return validate(node, target, root, path, allowedExtra)
	}
	var out []violation
	if t, ok := sch["type"].(string); ok {
		if err := checkType(node, t); err != nil {
			return append(out, violation{path, err.Error()})
		}
	}
	if c, ok := sch["const"]; ok {
		if !equalJSON(node, c) {
			return append(out, violation{path, fmt.Sprintf("must be constant %s", mustMarshal(c))})
		}
	}
	if enums, ok := sch["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			if equalJSON(node, e) {
				found = true
				break
			}
		}
		if !found {
			return append(out, violation{path, fmt.Sprintf("must be one of %s", mustMarshal(enums))})
		}
	}
	switch typed := node.(type) {
	case string:
		if n, ok := sch["minLength"].(float64); ok && int(len([]rune(typed))) < int(n) {
			out = append(out, violation{path, fmt.Sprintf("string length %d < minLength %d", len([]rune(typed)), int(n))})
		}
		if n, ok := sch["maxLength"].(float64); ok && len([]rune(typed)) > int(n) {
			out = append(out, violation{path, fmt.Sprintf("string length %d > maxLength %d", len([]rune(typed)), int(n))})
		}
		if p, ok := sch["pattern"].(string); ok {
			re, err := regexp.Compile(p)
			if err != nil {
				out = append(out, violation{path, fmt.Sprintf("schema error: bad pattern %q: %v", p, err)})
			} else if !re.MatchString(typed) {
				out = append(out, violation{path, fmt.Sprintf("does not match pattern %q", p)})
			}
		}
	case map[string]any:
		props, _ := sch["properties"].(map[string]any)
		required, _ := sch["required"].([]any)
		for _, r := range required {
			key, _ := r.(string)
			if _, present := typed[key]; !present {
				out = append(out, violation{path, fmt.Sprintf("missing required property %q", key)})
			}
		}
		additional, hasAdditional := sch["additionalProperties"]
		for key, value := range typed {
			childPath := path + "." + key
			if sub, ok := props[key].(map[string]any); ok {
				out = append(out, validate(value, sub, root, childPath, nil)...)
				continue
			}
			if !hasAdditional {
				continue
			}
			switch adj := additional.(type) {
			case bool:
				if !adj {
					if allowedExtra != nil && allowedExtra[key] && path == "$" {
						out = append(out, violation{path, fmt.Sprintf("tolerated extra property %q (not in schema; allowed via -allow-extra)", key)})
					} else {
						out = append(out, violation{path, fmt.Sprintf("unknown property %q (schema forbids extra properties)", key)})
					}
				}
			case map[string]any:
				out = append(out, validate(value, adj, root, childPath, nil)...)
			}
		}
	case []any:
		if n, ok := sch["minItems"].(float64); ok && len(typed) < int(n) {
			out = append(out, violation{path, fmt.Sprintf("has %d item(s) < minItems %d", len(typed), int(n))})
		}
		if uniq, ok := sch["uniqueItems"].(bool); ok && uniq {
			seen := map[string]bool{}
			for _, item := range typed {
				key := mustMarshal(item)
				if seen[key] {
					out = append(out, violation{path, "array items are not unique"})
					break
				}
				seen[key] = true
			}
		}
		if items, ok := sch["items"].(map[string]any); ok {
			for i, item := range typed {
				out = append(out, validate(item, items, root, fmt.Sprintf("%s[%d]", path, i), nil)...)
			}
		}
	}
	return out
}

func resolveRef(root map[string]any, ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("only local #/ refs are supported, got %q", ref)
	}
	cur := any(root)
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ref %q walks through a non-object at %q", ref, seg)
		}
		next, present := obj[seg]
		if !present {
			return nil, fmt.Errorf("ref %q target not found", ref)
		}
		cur = next
	}
	target, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ref %q target is not a schema object", ref)
	}
	return target, nil
}

func checkType(node any, want string) error {
	got := "null"
	switch node.(type) {
	case map[string]any:
		got = "object"
	case []any:
		got = "array"
	case string:
		got = "string"
	case bool:
		got = "boolean"
	case float64:
		got = "number"
		if want == "integer" && node.(float64) != float64(int64(node.(float64))) {
			return fmt.Errorf("want %s, got non-integer number", want)
		}
	}
	if got != want && !(want == "number" && got == "integer") {
		return fmt.Errorf("want type %s, got %s", want, got)
	}
	return nil
}

func equalJSON(a, b any) bool {
	return mustMarshal(a) == mustMarshal(b)
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable:%v>", v)
	}
	return string(b)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "validate_manifest: "+format+"\n", args...)
	os.Exit(2)
}
