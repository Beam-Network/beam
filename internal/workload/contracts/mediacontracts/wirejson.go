package mediacontracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
)

func MarshalWire(value any) ([]byte, error) {
	normalized := reflect.New(reflect.TypeOf(value)).Elem()
	normalized.Set(reflect.ValueOf(value))
	copied := deepCopyFillSlices(normalized)
	return marshalCanonical(copied.Interface())
}

func deepCopyFillSlices(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(deepCopyFillSlices(value.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		for index := 0; index < value.NumField(); index++ {
			if out.Field(index).CanSet() {
				out.Field(index).Set(deepCopyFillSlices(value.Field(index)))
			}
		}
		return out
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			out.Index(index).Set(deepCopyFillSlices(value.Index(index)))
		}
		return out
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			out.SetMapIndex(iterator.Key(), deepCopyFillSlices(iterator.Value()))
		}
		return out
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(deepCopyFillSlices(value.Elem()))
		return out
	default:
		return value
	}
}

var ErrStrictJSON = errors.New("mediacontracts: JSON does not match the contract shape exactly")

func UnmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return ErrStrictJSON
	}
	again, err := MarshalWire(reflect.ValueOf(target).Elem().Interface())
	if err != nil {
		return err
	}
	if !SameJSON(data, again) {
		return ErrStrictJSON
	}
	return nil
}

func SameJSON(left, right []byte) bool {
	a, errA := decodeAny(left)
	b, errB := decodeAny(right)
	return errA == nil && errB == nil && reflect.DeepEqual(a, b)
}

func decodeAny(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return normalizeNumbers(value), nil
}

func normalizeNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeNumbers(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = normalizeNumbers(item)
		}
		return typed
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return string(typed)
		}
		return number
	default:
		return value
	}
}
