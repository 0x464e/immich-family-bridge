//go:build windows

package httpapi

import "os"

func inodeDetails(os.FileInfo) map[string]any { return nil }
