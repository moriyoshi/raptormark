package builder

import "fmt"

// UsageError is a bad-invocation error. The shell scripts exited 2 for these and
// 1 for a failed step; cmd/raptormark preserves the distinction.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}
