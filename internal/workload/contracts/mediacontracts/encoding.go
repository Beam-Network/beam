package mediacontracts

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

func B64URL(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func DecodeB64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, err
	}
	if B64URL(decoded) != value {
		return nil, errors.New("non-canonical base64url")
	}
	return decoded, nil
}

func DecodeHex(value string) ([]byte, error) {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return nil, errors.New("hex must be lowercase")
		}
	}
	return hex.DecodeString(value)
}

func marshalCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func jsonInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed != math.Trunc(parsed) ||
		parsed < 0 || parsed > 9007199254740991 {
		return 0, false
	}
	return int64(parsed), true
}

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("trailing data after JSON value")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("JSON value is not an object")
	}
	return object, nil
}

func topLevelDuplicateKey(data []byte) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false, errors.New("not an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false, err
		}
		key, ok := token.(string)
		if !ok {
			return false, errors.New("bad key")
		}
		if seen[key] {
			return true, nil
		}
		seen[key] = true
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return false, err
		}
	}
	return false, nil
}
