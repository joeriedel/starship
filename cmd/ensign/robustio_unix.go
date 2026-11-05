// Copyright (c) 2026 Joseph Riedel
// Licensed under the MIT License. See LICENSE.

//go:build !windows

package main

func isTransientFileError(error) bool { return false }

func isTransientExistingFileError(error) bool { return false }
