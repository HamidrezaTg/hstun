package httpmode

import (
	"errors"
	"io"
)

var (
	errEOF    = io.EOF
	errClosed = errors.New("hstun http session closed")
)
