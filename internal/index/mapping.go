package index

import (
	"encoding/json"
	"strings"
	"sync"
)

// Field types understood by stretchy. Anything else declared in a
// mapping is accepted and treated as the closest match.
const (
	TypeText    = "text"
	TypeKeyword = "keyword"
	TypeLong    = "long"
	TypeDouble  = "double"
	TypeBool    = "boolean"
	TypeDate    = "date"
	TypeObject  = "object"
	TypeNested  = "nested"
)

type FieldMapping struct {
	Type       string                   `json:"type,omitempty"`
	Properties map[string]*FieldMapping `json:"properties,omitempty"`
	Fields     map[string]*FieldMapping `json:"fields,omitempty"`
	// Extra keeps declared options we don't act on (analyzer, format,
	// ignore_above, ...) so GET _mapping round-trips what clients sent.
	Extra map[string]json.RawMessage `json:"-"`
}

func (f *FieldMapping) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{}
	for k, v := range f.Extra {
		out[k] = v
	}
	if f.Type != "" {
		out["type"] = f.Type
	}
	if len(f.Properties) > 0 {
		out["properties"] = f.Properties
	}
	if len(f.Fields) > 0 {
		out["fields"] = f.Fields
	}
	return json.Marshal(out)
}

func (f *FieldMapping) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Extra = map[string]json.RawMessage{}
	for k, v := range raw {
		switch k {
		case "type":
			if err := json.Unmarshal(v, &f.Type); err != nil {
				return err
			}
		case "properties":
			if err := json.Unmarshal(v, &f.Properties); err != nil {
				return err
			}
		case "fields":
			if err := json.Unmarshal(v, &f.Fields); err != nil {
				return err
			}
		default:
			f.Extra[k] = v
		}
	}
	return nil
}

// Mapping is the per-index field type registry. Leaf lookups use
// dot-separated paths; multi-fields are addressed as path.subfield.
type Mapping struct {
	mu    sync.RWMutex
	Props map[string]*FieldMapping
	// leafCache flattens Props to dot-path -> type for fast lookups.
	leafCache map[string]string
}

func NewMapping() *Mapping {
	return &Mapping{Props: map[string]*FieldMapping{}, leafCache: map[string]string{}}
}

func (m *Mapping) UnmarshalBody(body json.RawMessage) error {
	var outer struct {
		Properties map[string]*FieldMapping `json:"properties"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, fm := range outer.Properties {
		m.Props[name] = fm
	}
	m.rebuildCacheLocked()
	return nil
}

func (m *Mapping) MarshalBody() json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, _ := json.Marshal(map[string]interface{}{"properties": m.Props})
	return raw
}

func (m *Mapping) rebuildCacheLocked() {
	m.leafCache = map[string]string{}
	var walk func(prefix string, props map[string]*FieldMapping)
	walk = func(prefix string, props map[string]*FieldMapping) {
		for name, fm := range props {
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			if len(fm.Properties) > 0 {
				walk(path, fm.Properties)
				continue
			}
			t := fm.Type
			if t == "" || t == TypeObject || t == TypeNested {
				continue
			}
			m.leafCache[path] = normalizeType(t)
			for sub, sfm := range fm.Fields {
				m.leafCache[path+"."+sub] = normalizeType(sfm.Type)
			}
		}
	}
	walk("", m.Props)
}

func normalizeType(t string) string {
	switch t {
	case "integer", "short", "byte", "long", "unsigned_long":
		return TypeLong
	case "float", "double", "half_float", "scaled_float":
		return TypeDouble
	case "string": // legacy ES 2.x
		return TypeText
	case "":
		return TypeText
	default:
		return t
	}
}

// FieldType returns the concrete type of a dot-path field, or "" when
// the field is not mapped.
func (m *Mapping) FieldType(path string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leafCache[path]
}

// EnsureDynamic registers a field discovered while indexing, mirroring
// Elasticsearch dynamic mapping: strings become text with a .keyword
// multi-field, numbers long/double, bools boolean, and strings that
// parse as dates become date.
func (m *Mapping) EnsureDynamic(path string, value interface{}) string {
	if t := m.FieldType(path); t != "" {
		return t
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.leafCache[path]; ok {
		return t
	}
	var fm *FieldMapping
	var leafType string
	switch v := value.(type) {
	case string:
		if _, ok := ParseDate(v); ok {
			leafType = TypeDate
			fm = &FieldMapping{Type: TypeDate}
		} else {
			leafType = TypeText
			fm = &FieldMapping{Type: TypeText, Fields: map[string]*FieldMapping{
				"keyword": {Type: TypeKeyword},
			}}
		}
	case bool:
		leafType = TypeBool
		fm = &FieldMapping{Type: TypeBool}
	case json.Number:
		leafType = TypeLong
		if strings.ContainsAny(v.String(), ".eE") {
			leafType = TypeDouble
		}
		fm = &FieldMapping{Type: leafType}
	case float64:
		leafType = TypeDouble
		fm = &FieldMapping{Type: TypeDouble}
	default:
		return ""
	}

	// Insert at the right nesting level, creating object parents.
	parts := strings.Split(path, ".")
	props := m.Props
	for i := 0; i < len(parts)-1; i++ {
		parent, ok := props[parts[i]]
		if !ok {
			parent = &FieldMapping{Properties: map[string]*FieldMapping{}}
			props[parts[i]] = parent
		}
		if parent.Properties == nil {
			parent.Properties = map[string]*FieldMapping{}
		}
		props = parent.Properties
	}
	props[parts[len(parts)-1]] = fm

	m.leafCache[path] = leafType
	if leafType == TypeText {
		m.leafCache[path+".keyword"] = TypeKeyword
	}
	return leafType
}

// LeafFields returns a copy of the flattened path -> type map.
func (m *Mapping) LeafFields() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.leafCache))
	for k, v := range m.leafCache {
		out[k] = v
	}
	return out
}

// TextParent resolves "field.keyword" -> "field" when the sub-field is
// a keyword multi-field of a text field. Returns "" when path is not a
// multi-field.
func (m *Mapping) TextParent(path string) string {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return ""
	}
	parent := path[:i]
	if m.FieldType(parent) != "" {
		return parent
	}
	return ""
}
