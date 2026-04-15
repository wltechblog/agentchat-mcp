package filestore

import "errors"

var ErrFileTooLarge = errors.New("file exceeds maximum allowed size")
