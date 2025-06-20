/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watchdog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNew tests the New function with various scenarios
func TestNew(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		setup       func() (string, func())
		expectError bool
		errorMsg    string
	}{
		{
			name:        "empty path",
			path:        "",
			expectError: true,
			errorMsg:    "watchdog device path cannot be empty",
		},
		{
			name:        "non-existent path",
			path:        "/dev/non-existent-watchdog",
			expectError: true,
			errorMsg:    "failed to open watchdog device",
		},
		{
			name: "valid path with mock file",
			setup: func() (string, func()) {
				// Create a temporary file to simulate a watchdog device
				tmpDir := t.TempDir()
				tmpFile := filepath.Join(tmpDir, "mock_watchdog")
				file, err := os.Create(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create mock file: %v", err)
				}
				file.Close()

				return tmpFile, func() {
					os.Remove(tmpFile)
				}
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			var cleanup func()

			if tt.setup != nil {
				path, cleanup = tt.setup()
				defer cleanup()
			} else {
				path = tt.path
			}

			wd, err := New(path)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got: %s", tt.errorMsg, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if wd == nil {
				t.Error("Expected watchdog instance but got nil")
				return
			}

			// Verify watchdog properties
			if !wd.IsOpen() {
				t.Error("Expected watchdog to be open")
			}

			if wd.Path() != path {
				t.Errorf("Expected path %s, got %s", path, wd.Path())
			}

			// Clean up
			if err := wd.Close(); err != nil {
				t.Errorf("Failed to close watchdog: %v", err)
			}
		})
	}
}

// TestPet tests the Pet method with various scenarios
func TestPet(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() *Watchdog
		expectError bool
		errorMsg    string
	}{
		{
			name: "pet closed watchdog",
			setup: func() *Watchdog {
				tmpDir := t.TempDir()
				tmpFile := filepath.Join(tmpDir, "mock_watchdog")
				file, err := os.Create(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create mock file: %v", err)
				}
				file.Close()

				wd, err := New(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create watchdog: %v", err)
				}

				// Close the watchdog to test petting a closed device
				wd.Close()
				return wd
			},
			expectError: true,
			errorMsg:    "watchdog device is not open",
		},
		{
			name: "pet with nil file descriptor",
			setup: func() *Watchdog {
				// Create a watchdog with nil file descriptor
				return &Watchdog{
					file:   nil,
					path:   "/test/path",
					isOpen: true, // Mark as open but with nil file
				}
			},
			expectError: true,
			errorMsg:    "watchdog file descriptor is nil",
		},
		{
			name: "pet valid watchdog (will fail ioctl on regular file)",
			setup: func() *Watchdog {
				tmpDir := t.TempDir()
				tmpFile := filepath.Join(tmpDir, "mock_watchdog")
				file, err := os.Create(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create mock file: %v", err)
				}
				file.Close()

				wd, err := New(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create watchdog: %v", err)
				}
				return wd
			},
			expectError: true,
			errorMsg:    "failed to pet watchdog", // ioctl will fail on regular file
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wd := tt.setup()
			defer func() {
				if wd.IsOpen() {
					wd.Close()
				}
			}()

			err := wd.Pet()

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got: %s", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

// TestClose tests the Close method with various scenarios
func TestClose(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() *Watchdog
		expectError bool
		errorMsg    string
	}{
		{
			name: "close already closed watchdog",
			setup: func() *Watchdog {
				tmpDir := t.TempDir()
				tmpFile := filepath.Join(tmpDir, "mock_watchdog")
				file, err := os.Create(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create mock file: %v", err)
				}
				file.Close()

				wd, err := New(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create watchdog: %v", err)
				}

				// Close it once first
				wd.Close()
				return wd
			},
			expectError: false, // Should not error on double close
		},
		{
			name: "close watchdog with nil file",
			setup: func() *Watchdog {
				return &Watchdog{
					file:   nil,
					path:   "/test/path",
					isOpen: true,
				}
			},
			expectError: false,
		},
		{
			name: "close valid watchdog",
			setup: func() *Watchdog {
				tmpDir := t.TempDir()
				tmpFile := filepath.Join(tmpDir, "mock_watchdog")
				file, err := os.Create(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create mock file: %v", err)
				}
				file.Close()

				wd, err := New(tmpFile)
				if err != nil {
					t.Fatalf("Failed to create watchdog: %v", err)
				}
				return wd
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wd := tt.setup()

			err := wd.Close()

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got: %s", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// After closing, watchdog should not be open
			if wd.IsOpen() {
				t.Error("Expected watchdog to be closed after Close()")
			}
		})
	}
}

// TestWatchdogProperties tests the IsOpen and Path methods
func TestWatchdogProperties(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "mock_watchdog")

	// Create a mock file
	file, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create mock file: %v", err)
	}
	file.Close()

	wd, err := New(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create watchdog: %v", err)
	}

	// Test initial state
	if !wd.IsOpen() {
		t.Error("Expected new watchdog to be open")
	}

	if wd.Path() != tmpFile {
		t.Errorf("Expected path %s, got %s", tmpFile, wd.Path())
	}

	// Test after closing
	err = wd.Close()
	if err != nil {
		t.Fatalf("Failed to close watchdog: %v", err)
	}

	if wd.IsOpen() {
		t.Error("Expected watchdog to be closed after Close()")
	}

	// Path should still be available after closing
	if wd.Path() != tmpFile {
		t.Errorf("Expected path %s after close, got %s", tmpFile, wd.Path())
	}
}

// TestWatchdogLifecycle tests the complete lifecycle of a watchdog
func TestWatchdogLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "mock_watchdog")

	// Create a mock file
	file, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create mock file: %v", err)
	}
	file.Close()

	// Create watchdog
	wd, err := New(tmpFile)
	if err != nil {
		t.Fatalf("Failed to create watchdog: %v", err)
	}

	// Verify initial state
	if !wd.IsOpen() {
		t.Error("Expected new watchdog to be open")
	}

	// Try to pet (will fail with ioctl error on regular file, but that's expected)
	err = wd.Pet()
	if err == nil {
		t.Error("Expected pet to fail on regular file")
	}

	// Close watchdog
	err = wd.Close()
	if err != nil {
		t.Errorf("Failed to close watchdog: %v", err)
	}

	// Verify closed state
	if wd.IsOpen() {
		t.Error("Expected watchdog to be closed")
	}

	// Try to pet closed watchdog
	err = wd.Pet()
	if err == nil {
		t.Error("Expected pet to fail on closed watchdog")
	}
	if !strings.Contains(err.Error(), "watchdog device is not open") {
		t.Errorf("Expected specific error message, got: %s", err.Error())
	}

	// Double close should not error
	err = wd.Close()
	if err != nil {
		t.Errorf("Double close should not error: %v", err)
	}
}
