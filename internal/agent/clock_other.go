//go:build !linux

package agent

import (
	"errors"
	"time"
)

func ntpSynced() bool { return true }

func setSystemClock(time.Time) error { return errors.New("not supported on this OS") }
