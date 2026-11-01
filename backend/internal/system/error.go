package system

import "fmt"

// Error 是可返回给调用方的业务错误。
type Error struct {
	Code    string
	Message string
	HTTP    int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errCode(code, message string, http int) *Error {
	return &Error{Code: code, Message: message, HTTP: http}
}

func IsCode(err error, code string) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*Error)
	return ok && e.Code == code
}

func wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}
