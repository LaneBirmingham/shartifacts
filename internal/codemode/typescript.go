package codemode

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Signature renders an operation as a TypeScript method declaration: its description as a
// doc comment, then its input from the JSON schema, with each field's description above
// it. Models read TypeScript more readily than JSON Schema, which is why Code Mode offers
// it; the schema stays in the spec beside it for the details TypeScript cannot say.
func Signature(name, description string, input *jsonschema.Schema) string {
	var b strings.Builder
	if description != "" {
		b.WriteString(docComment(description, ""))
	}
	b.WriteString(name)
	b.WriteString("(input")
	if input == nil || len(input.Required) == 0 {
		b.WriteString("?")
	}
	b.WriteString(": ")
	b.WriteString(tsType(input, ""))
	b.WriteString("): Promise<any>")
	return b.String()
}

func docComment(text, indent string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "*/", "*\\/")
	return indent + "/** " + text + " */\n"
}

// tsType renders a schema as a TypeScript type, indenting nested objects under indent.
func tsType(s *jsonschema.Schema, indent string) string {
	if s == nil {
		return "any"
	}
	if len(s.Enum) > 0 {
		parts := make([]string, len(s.Enum))
		for i, v := range s.Enum {
			b, _ := json.Marshal(v)
			parts[i] = string(b)
		}
		return strings.Join(parts, " | ")
	}
	for _, list := range [][]*jsonschema.Schema{s.AnyOf, s.OneOf} {
		if len(list) > 0 {
			parts := make([]string, len(list))
			for i, alt := range list {
				parts[i] = tsType(alt, indent)
			}
			return strings.Join(parts, " | ")
		}
	}
	types := s.Types
	if s.Type != "" {
		types = []string{s.Type}
	}
	if len(types) == 0 {
		if len(s.Properties) > 0 {
			types = []string{"object"}
		} else {
			return "any"
		}
	}
	parts := []string{}
	for _, t := range types {
		switch t {
		case "string", "boolean", "null":
			parts = append(parts, t)
		case "integer", "number":
			parts = append(parts, "number")
		case "array":
			item := tsType(s.Items, indent)
			if strings.Contains(item, " | ") {
				item = "(" + item + ")"
			}
			parts = append(parts, item+"[]")
		case "object":
			parts = append(parts, objectType(s, indent))
		default:
			parts = append(parts, "any")
		}
	}
	return strings.Join(parts, " | ")
}

func objectType(s *jsonschema.Schema, indent string) string {
	if len(s.Properties) == 0 {
		if s.AdditionalProperties == nil || isFalse(s.AdditionalProperties) {
			if s.AdditionalProperties == nil {
				return "{ [key: string]: any }"
			}
			return "{}"
		}
		return "{ [key: string]: " + tsType(s.AdditionalProperties, indent) + " }"
	}
	order := s.PropertyOrder
	if len(order) != len(s.Properties) {
		order = order[:0:0]
		for name := range s.Properties {
			order = append(order, name)
		}
		sort.Strings(order)
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	inner := indent + "  "
	var b strings.Builder
	b.WriteString("{\n")
	for _, name := range order {
		p := s.Properties[name]
		if p == nil {
			continue
		}
		if p.Description != "" {
			b.WriteString(docComment(p.Description, inner))
		}
		b.WriteString(inner)
		b.WriteString(name)
		if !required[name] {
			b.WriteString("?")
		}
		b.WriteString(": ")
		if !required[name] {
			p = withoutNull(p)
		}
		b.WriteString(tsType(p, inner))
		b.WriteString(";\n")
	}
	b.WriteString(indent)
	b.WriteString("}")
	return b.String()
}

// withoutNull drops null from an optional property's types: inference makes every pointer,
// slice and map nullable, and to an operation an omitted field and a null one are the same.
func withoutNull(s *jsonschema.Schema) *jsonschema.Schema {
	if len(s.Types) < 2 {
		return s
	}
	c := *s
	c.Types = nil
	for _, t := range s.Types {
		if t != "null" {
			c.Types = append(c.Types, t)
		}
	}
	return &c
}

// isFalse reports the schema that matches nothing, which inference uses to forbid
// properties a struct does not have.
func isFalse(s *jsonschema.Schema) bool {
	return s.Not != nil && s.Not.Type == "" && len(s.Not.Types) == 0 && len(s.Not.Properties) == 0
}
