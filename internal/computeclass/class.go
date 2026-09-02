package computeclass

import (
	"fmt"
	"strings"
)

const UnsupportedCode = "unsupported_compute_class"

var supported = [...]string{"c1", "c2", "c3"}

// Supported returns the ordered, provider-neutral compute classes exposed by
// Canter. The order is significant: providers resolve c1 to their smallest
// supported shape, c2 to the next shape, and so on.
func Supported() []string {
	classes := make([]string, len(supported))
	copy(classes, supported[:])
	return classes
}

// Index returns the provider shape ordinal for a Canter compute class.
func Index(class string) (int, bool) {
	for index, candidate := range supported {
		if class == candidate {
			return index, true
		}
	}
	return 0, false
}

func Validate(class string) error {
	if _, ok := Index(class); ok {
		return nil
	}
	return &UnsupportedClassError{Class: class}
}

type UnsupportedClassError struct {
	Class string
}

func (e *UnsupportedClassError) Error() string {
	return fmt.Sprintf("%s: host class %q is unsupported; supported classes: %s", UnsupportedCode, e.Class, strings.Join(supported[:], ", "))
}

func UnsupportedError(class string) error {
	return &UnsupportedClassError{Class: class}
}

// IsSafePublicFailure identifies a domain validation failure that contains
// only agent-supplied input and Canter's public class names. Provider errors
// and execution details must continue through the generic redaction path.
func IsSafePublicFailure(message string) bool {
	return strings.HasPrefix(message, UnsupportedCode+": ")
}
