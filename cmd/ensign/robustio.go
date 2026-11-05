// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

package main

import (
	"os"
	"time"
)

// retryWindow bounds how long a file operation keeps retrying a transient error.
const retryWindow = 2 * time.Second

// retry runs op until it succeeds, fails with an error transient rejects, or window passes; it then returns the last error unchanged. It sleeps 1ms between tries, doubling up to 100ms.
func retry(window time.Duration, transient func(error) bool, op func() error) error {
	start := time.Now()
	delay := time.Millisecond
	for {
		err := op()
		if err == nil || !transient(err) || time.Since(start) >= window {
			return err
		}
		time.Sleep(delay)
		delay = min(delay*2, 100*time.Millisecond)
	}
}

// renameRetry is os.Rename, retried while another process briefly holds a file open on Windows.
func renameRetry(oldpath, newpath string) error {
	return retry(retryWindow, isTransientFileError, func() error { return os.Rename(oldpath, newpath) })
}

// readExistingFileRetry is readFileRetry for a file known to exist, so it also retries a not-found that Windows reports while a save replaces the file.
func readExistingFileRetry(path string) ([]byte, error) {
	var data []byte
	err := retry(retryWindow, isTransientExistingFileError, func() (err error) {
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}

// readExistingJSON is readJSON for a file known to exist.
func readExistingJSON(path string, v any) error {
	data, err := readExistingFileRetry(path)
	if err != nil {
		return err
	}
	return unmarshalFile(path, data, v)
}

// readFileRetry is os.ReadFile, retried while a save briefly holds the file on Windows.
func readFileRetry(path string) ([]byte, error) {
	var data []byte
	err := retry(retryWindow, isTransientFileError, func() (err error) {
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}
