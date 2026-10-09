package service

import (
	"encoding/json"
	"reflect"
	"strings"
)

func outputSchema(value any) map[string]any {
	defs := map[string]any{}
	root := schemaType(reflect.TypeOf(value), defs)
	root["type"] = "object"
	root["$defs"] = defs
	return root
}
func schemaType(t reflect.Type, defs map[string]any) map[string]any {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{schemaType(t.Elem(), defs), map[string]any{"type": "null"}}}
	}
	switch t.Kind() {
	case reflect.Struct:
		name := t.Name()
		if name != "" {
			if _, ok := defs[name]; ok {
				return map[string]any{"$ref": "#/$defs/" + name}
			}
			defs[name] = map[string]any{}
		}
		properties := map[string]any{}
		required := []string{}
		var fields func(reflect.Type)
		fields = func(t reflect.Type) {
			for n := 0; n < t.NumField(); n++ {
				f := t.Field(n)
				if !f.IsExported() {
					continue
				}
				tag := strings.Split(f.Tag.Get("json"), ",")
				if tag[0] == "-" {
					continue
				}
				if f.Anonymous && tag[0] == "" {
					fields(f.Type)
					continue
				}
				key := tag[0]
				if key == "" {
					key = f.Name
				}
				properties[key] = schemaType(f.Type, defs)
				optional := false
				for _, option := range tag[1:] {
					if option == "omitempty" {
						optional = true
					}
				}
				if !optional {
					required = append(required, key)
				}
			}
		}
		fields(t)
		s := map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
		if name != "" {
			defs[name] = s
			return map[string]any{"$ref": "#/$defs/" + name}
		}
		return s
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": []string{"array", "null"}, "items": schemaType(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": schemaType(t.Elem(), defs)}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}
