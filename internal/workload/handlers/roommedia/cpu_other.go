//go:build !unix

package roommedia

import "time"

func processCPU() (time.Duration, bool) { return 0, false }
