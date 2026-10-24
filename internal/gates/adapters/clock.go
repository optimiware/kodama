package adapters

import "time"

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
