//go:build !windows

package main

func isTransientFileError(error) bool { return false }

func isTransientExistingFileError(error) bool { return false }
