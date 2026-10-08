package audit

import (
	"encoding"
	"encoding/json"
	"reflect"
	"time"
)

var jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
var textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
var auditTimeType = reflect.TypeFor[time.Time]()
var auditRawJSONType = reflect.TypeFor[json.RawMessage]()

// Preflight bounds the input graph before any JSON allocation or user-defined
// encoding callback. Wire validation remains authoritative afterward. Audit
// metadata accepts plain JSON values, finite structs, RawMessage and time.Time;
// arbitrary marshal callbacks cannot establish a bounded working set.
func (e Event) Preflight(limit int) error {
	if limit <= 0 {
		return ErrBatchLimit
	}
	remaining := limit
	for _, value := range []string{e.EventID, e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID, e.Outcome, e.ReasonCode, e.CorrelationID} {
		if len(value) > remaining {
			return ErrBatchLimit
		}
		remaining -= len(value)
	}
	bytes, nodes := 8<<10, 8<<10
	if remaining < bytes {
		bytes = remaining
	}
	if !auditMetadataBound(reflect.ValueOf(e.Metadata), 0, &bytes, &nodes) {
		return ErrBatchLimit
	}
	return nil
}

func auditMetadataBound(v reflect.Value, depth int, bytes, nodes *int) bool {
	if !v.IsValid() {
		return true
	}
	if depth > 8<<10 || *nodes < 1 || *bytes < 1 {
		return false
	}
	*nodes--
	*bytes--
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return true
		}
		if v.Kind() == reflect.Pointer && v.Type().Elem() == auditTimeType {
			return true
		}
		// A custom pointer callback is refused before dereferencing it.
		if v.Kind() == reflect.Pointer && v.Type().Elem() != auditRawJSONType && auditCustomMarshaler(v.Type()) {
			return false
		}
		return auditMetadataBound(v.Elem(), depth+1, bytes, nodes)
	}
	if v.Type() == auditTimeType {
		return true
	}
	if v.Type() != auditRawJSONType && auditCustomMarshaler(v.Type()) {
		return false
	}
	take := func(n int) bool {
		if n > *bytes {
			return false
		}
		*bytes -= n
		return true
	}
	switch v.Kind() {
	case reflect.String:
		return take(v.Len())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return take(v.Len())
		}
		if v.Len() > *nodes {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if !auditMetadataBound(v.Index(i), depth+1, bytes, nodes) {
				return false
			}
		}
		return true
	case reflect.Map:
		if v.Len() > *nodes/2 {
			return false
		}
		iter := v.MapRange()
		for iter.Next() {
			if !auditMetadataBound(iter.Key(), depth+1, bytes, nodes) || !auditMetadataBound(iter.Value(), depth+1, bytes, nodes) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if v.NumField() > *nodes {
			return false
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			if !take(len(field.Name)) || !auditMetadataBound(v.Field(i), depth+1, bytes, nodes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func auditCustomMarshaler(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}
