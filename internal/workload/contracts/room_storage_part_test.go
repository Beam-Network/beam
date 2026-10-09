package contracts

import "testing"

// Room storage parts follow the standard consecutive multipart contract that BeamCore
// and the hybrid route issuer use: part = chunk index + 1 for every attempt.
func TestMultipartPartNumberIsConsecutive(t *testing.T) {
	for _, test := range []struct{ chunk, part int64 }{{0, 1}, {1, 2}, {13, 14}, {9999, 10000}, {10000, 0}, {-1, 0}} {
		if got := MultipartPartNumber(test.chunk); got != test.part {
			t.Fatalf("MultipartPartNumber(%d) = %d, want %d", test.chunk, got, test.part)
		}
	}
}
