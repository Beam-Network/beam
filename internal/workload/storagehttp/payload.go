package storagehttp

import (
	"errors"
	"io"
)

// ReadExact allocates exactly the admitted payload rather than growing and
// copying it, and fails when the source is shorter or longer than length.
func ReadExact(reader io.Reader, length int64) ([]byte, error) {
	if length <= 0 {
		return nil, errors.New("invalid source payload length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	var extra [1]byte
	if _, err := io.ReadFull(reader, extra[:]); err != io.EOF {
		return nil, errors.New("source payload exceeds admitted length")
	}
	return payload, nil
}
