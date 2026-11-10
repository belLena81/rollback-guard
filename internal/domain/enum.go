package domain

import "fmt"

// unknownName is the String form of every enumeration value outside its range.
const unknownName = "unknown"

// enumValue is a closed, contiguous uint8 enumeration whose String form is its
// canonical spelling. Zero is never a member.
type enumValue interface {
	~uint8
	String() string
}

// parseEnum returns the member of first..last whose String equals value. It is
// the inverse of String for every enumeration in this package.
func parseEnum[T enumValue](name, value string, first, last T) (T, error) {
	for member := first; member <= last; member++ {
		if member.String() == value {
			return member, nil
		}
	}
	return 0, fmt.Errorf("unknown %s %q", name, value)
}
