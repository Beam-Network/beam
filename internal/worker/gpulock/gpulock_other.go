//go:build !unix

package gpulock

import "errors"

const Supported = false

func Prepare(string) error { return errors.New("GPU locks need a Unix Worker") }

func Exclusive(string, int64, int64) ([]string, func(), error) {
	return nil, nil, errors.New("GPU actions need a Unix Worker")
}

func Shared(string, int) (*Lock, error) { return &Lock{}, nil }
