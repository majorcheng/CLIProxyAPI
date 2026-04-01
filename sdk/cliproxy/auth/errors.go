package auth

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Error describes an authentication related failure in a provider agnostic format.
type Error struct {
	// Code is a short machine readable identifier.
	Code string `json:"code,omitempty"`
	// Message is a human readable description of the failure.
	Message string `json:"message"`
	// Retryable indicates whether a retry might fix the issue automatically.
	Retryable bool `json:"retryable"`
	// HTTPStatus optionally records an HTTP-like status code for the error.
	HTTPStatus int `json:"http_status,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// StatusCode implements optional status accessor for manager decision making.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

type authCapacityError struct {
	retryAfter time.Duration
}

func newAuthCapacityError(retryAfter time.Duration) *authCapacityError {
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &authCapacityError{retryAfter: retryAfter}
}

func (e *authCapacityError) Error() string {
	return "当前所有可用凭证已达到配置的并发上限"
}

func (e *authCapacityError) StatusCode() int {
	return http.StatusServiceUnavailable
}

func (e *authCapacityError) Headers() http.Header {
	if e == nil {
		return nil
	}
	headers := make(http.Header)
	retryAfter := e.retryAfter
	if retryAfter < 0 {
		retryAfter = 0
	}
	headers.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	return headers
}

func (e *authCapacityError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	retryAfter := e.retryAfter
	return &retryAfter
}

func isAuthCapacityError(err error) bool {
	var capacityErr *authCapacityError
	return err != nil && errors.As(err, &capacityErr)
}
