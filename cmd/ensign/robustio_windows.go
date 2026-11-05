// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientFileError reports a collision with another process's open handle. ERROR_FILE_NOT_FOUND is deliberately not one: a missing file must fail at once.
func isTransientFileError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// isTransientExistingFileError is isTransientFileError plus ERROR_FILE_NOT_FOUND, which a read can get while a save replaces a file that exists. ERROR_PATH_NOT_FOUND is left out so a deleted directory fails at once.
func isTransientExistingFileError(err error) bool {
	return isTransientFileError(err) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
