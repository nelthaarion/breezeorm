// Task 2 — pkg/xtypes/unixtime.go (new file)
// UnixTime stores a time.Time as an INTEGER (unix seconds) column instead
// of TEXT, removing the driver-side time.Parse call that showed up in the
// profile. Implements sql.Scanner/driver.Valuer directly, so it goes
// through the scan loop's existing kindOther path (a.assign ->
// reflect.NewAt(t, p).Interface() -> database/sql calls Scan(src) on it) —
// zero changes to ScanAllHint or classify().
package xtypes

import (
	"database/sql/driver"
	"fmt"
	"time"
)

type UnixTime time.Time

func (t *UnixTime) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*t = UnixTime(time.Unix(v, 0).UTC())
	case nil:
		*t = UnixTime(time.Time{})
	default:
		return fmt.Errorf("xtypes: UnixTime.Scan: unsupported source type %T", src)
	}
	return nil
}

func (t UnixTime) Value() (driver.Value, error) {
	tt := time.Time(t)
	if tt.IsZero() {
		return nil, nil
	}
	return tt.Unix(), nil
}

func (t UnixTime) Time() time.Time { return time.Time(t) }
