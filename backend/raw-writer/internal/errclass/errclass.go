// Package errclass names errors for logs without exposing their messages, which may
// contain event content or credentials.
package errclass

import (
	"errors"
	"reflect"

	"github.com/aws/smithy-go"
)

// Of names an error for logs without exposing its message: an ErrorClass() method in
// the chain wins (Python exception names), then the S3 API error code, then the Go type
// name of the innermost wrapped error.
func Of(err error) string {
	var named interface{ ErrorClass() string }
	if errors.As(err, &named) {
		return named.ErrorClass()
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() != "" {
		return apiErr.ErrorCode()
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() == "" {
		return t.String()
	}
	return t.Name()
}
