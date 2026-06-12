package storage

import "fmt"

var _ error = (*ErrorWithStatusCode)(nil)

type ErrorWithStatusCode struct {
	Err        error
	StatusCode int
}

func (e *ErrorWithStatusCode) Error() string {
	return fmt.Sprintf("Err: %s, Status Code: %d", e.Err, e.StatusCode)
}

func (e *ErrorWithStatusCode) UnWrap() error {
	return e.Err
}
