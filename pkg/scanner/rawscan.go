// pkg/scanner/rawscan.go
package scanner

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// ErrRawUnsupported is returned (and should trigger fallback to the normal
// database/sql-based ScanAllHint) when the driver doesn't expose
// driver.QueryerContext, or the Plan contains a column type the raw path
// doesn't handle (anything outside the 6 native driver.Value kinds).
var ErrRawUnsupported = errors.New("scanner: raw path unsupported")

// rawCapable reports whether every column in the plan maps directly onto one
// of the driver's 6 native Value kinds, computed once per Plan and cached —
// never re-evaluated per row or per query.
func (p *Plan) rawCapable() bool {
	p.rawOnce.Do(func() {
		p.rawOK = true
		for i := range p.Assignments {
			switch p.Assignments[i].Kind {
			case kindInt64, kindFloat64, kindString, kindBytes, kindBool, kindTime:
			default:
				p.rawOK = false
				return
			}
		}
	})
	return p.rawOK
}

// writeRaw assigns one driver.Value directly into the struct field at
// fieldPtr, with no reflection and no intermediate `any` boxing beyond the
// driver.Value the driver itself produced. NULL for these non-pointer,
// non-Null* fields is an error, matching database/sql's own
// "converting NULL to <type> is unsupported" behavior.
func writeRaw(kind fieldKind, fieldPtr unsafe.Pointer, v driver.Value, col string) error {
	if v == nil {
		return fmt.Errorf("scanner: raw scan: NULL value for non-nullable column %q", col)
	}
	switch kind {
	case kindInt64:
		iv, ok := v.(int64)
		if !ok {
			return fmt.Errorf("scanner: raw scan: column %q: want int64, got %T", col, v)
		}
		*(*int64)(fieldPtr) = iv
	case kindFloat64:
		fv, ok := v.(float64)
		if !ok {
			return fmt.Errorf("scanner: raw scan: column %q: want float64, got %T", col, v)
		}
		*(*float64)(fieldPtr) = fv
	case kindBool:
		bv, ok := v.(bool)
		if !ok {
			return fmt.Errorf("scanner: raw scan: column %q: want bool, got %T", col, v)
		}
		*(*bool)(fieldPtr) = bv
	case kindTime:
		tv, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("scanner: raw scan: column %q: want time.Time, got %T", col, v)
		}
		*(*time.Time)(fieldPtr) = tv
	case kindString:
		switch sv := v.(type) {
		case string:
			*(*string)(fieldPtr) = sv
		case []byte:
			// Copy: driver-owned buffer, may be reused after Next returns.
			*(*string)(fieldPtr) = string(sv)
		default:
			return fmt.Errorf("scanner: raw scan: column %q: want string, got %T", col, v)
		}
	case kindBytes:
		switch bv := v.(type) {
		case []byte:
			b := make([]byte, len(bv))
			copy(b, bv)
			*(*[]byte)(fieldPtr) = b
		case string:
			*(*[]byte)(fieldPtr) = []byte(bv)
		default:
			return fmt.Errorf("scanner: raw scan: column %q: want []byte, got %T", col, v)
		}
	}
	return nil
}
