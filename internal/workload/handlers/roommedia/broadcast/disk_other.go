//go:build !unix

package broadcast

func FreeDiskBytes(string) (int64, bool) { return 0, false }
