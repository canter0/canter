package computeclass

import (
	"fmt"
	"strconv"
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

// NormalizePublicFailure identifies the current structured error and the exact
// legacy error emitted before unsupported classes were validated at ingress.
// Both contain only agent-supplied input and Canter's public class names.
func NormalizePublicFailure(message string) (string, bool) {
	if strings.HasPrefix(message, UnsupportedCode+": ") {
		return message, true
	}
	const legacyPrefix = "unsupported compute class "
	if !strings.HasPrefix(message, legacyPrefix) {
		return "", false
	}
	class, err := strconv.Unquote(strings.TrimPrefix(message, legacyPrefix))
	if err != nil {
		return "", false
	}
	return UnsupportedError(class).Error(), true
}

func IsSafePublicFailure(message string) bool {
	_, ok := NormalizePublicFailure(message)
	return ok
}
