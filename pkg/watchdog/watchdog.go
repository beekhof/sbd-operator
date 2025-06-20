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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/medik8s/sbd-operator/pkg/retry"
	"golang.org/x/sys/unix"
)

// Linux watchdog ioctl constants
// Reference: include/uapi/linux/watchdog.h
const (
	// WDIOC_KEEPALIVE is the ioctl command to reset/pet the watchdog timer
	// This is equivalent to _IO('W', 5) in C
	WDIOC_KEEPALIVE = 0x40045705

	// WDIOC_SETTIMEOUT is the ioctl command to set the watchdog timeout
	// This is equivalent to _IOWR('W', 6, int) in C
	WDIOC_SETTIMEOUT = 0x40045706

	// WDIOC_GETTIMEOUT is the ioctl command to get the watchdog timeout
	// This is equivalent to _IOR('W', 7, int) in C
	WDIOC_GETTIMEOUT = 0x40045707
)

// Retry configuration constants for watchdog operations
const (
	// MaxWatchdogRetries is the maximum number of retry attempts for watchdog operations
	MaxWatchdogRetries = 2
	// InitialWatchdogRetryDelay is the initial delay between watchdog retry attempts
	InitialWatchdogRetryDelay = 50 * time.Millisecond
	// MaxWatchdogRetryDelay is the maximum delay between watchdog retry attempts
	MaxWatchdogRetryDelay = 500 * time.Millisecond
	// WatchdogRetryBackoffFactor is the exponential backoff factor for watchdog retry delays
	WatchdogRetryBackoffFactor = 2.0
)

// Watchdog represents a Linux kernel watchdog device interface.
// It provides methods to interact with hardware watchdog devices through
// the Linux watchdog subsystem.
type Watchdog struct {
	// file is the open file descriptor for the watchdog device
	file *os.File
	// path is the filesystem path to the watchdog device
	path string
	// isOpen tracks whether the watchdog device is currently open
	isOpen bool
	// logger for logging watchdog operations and retries
	logger logr.Logger
	// retryConfig holds the retry configuration for this watchdog
	retryConfig retry.Config
}

// New creates a new Watchdog instance by opening the watchdog device at the specified path.
// Common paths include '/dev/watchdog' or '/dev/watchdog0'.
//
// Parameters:
//   - path: The filesystem path to the watchdog device (e.g., "/dev/watchdog")
//
// Returns:
//   - *Watchdog: A new Watchdog instance if successful
//   - error: An error if the device cannot be opened
//
// The device is opened with O_WRONLY flag as required by most watchdog devices.
// Once opened, the watchdog timer is typically activated and must be periodically
// reset using the Pet() method to prevent system reset.
func New(path string) (*Watchdog, error) {
	return NewWithLogger(path, logr.Discard())
}

// NewWithLogger creates a new Watchdog instance with a logger for retry operations
func NewWithLogger(path string, logger logr.Logger) (*Watchdog, error) {
	if path == "" {
		return nil, fmt.Errorf("watchdog device path cannot be empty")
	}

	// Configure retry settings for watchdog operations
	retryConfig := retry.Config{
		MaxRetries:    MaxWatchdogRetries,
		InitialDelay:  InitialWatchdogRetryDelay,
		MaxDelay:      MaxWatchdogRetryDelay,
		BackoffFactor: WatchdogRetryBackoffFactor,
		Logger:        logger.WithName("watchdog-retry"),
	}

	var file *os.File
	var err error

	// Retry watchdog opening for transient errors
	ctx := context.Background()
	err = retry.Do(ctx, retryConfig, "open watchdog device", func() error {
		// Open the watchdog device with write-only access
		// Most watchdog devices require write access for ioctl operations
		file, err = os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			// Wrap the error with additional context and retry information
			if pathErr, ok := err.(*os.PathError); ok {
				return retry.NewRetryableError(pathErr, retry.IsTransientError(pathErr), "open watchdog device")
			}
			return retry.NewRetryableError(err, retry.IsTransientError(err), "open watchdog device")
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to open watchdog device at %s: %w", path, err)
	}

	return &Watchdog{
		file:        file,
		path:        path,
		isOpen:      true,
		logger:      logger.WithName("watchdog").WithValues("path", path),
		retryConfig: retryConfig,
	}, nil
}

// Pet resets the watchdog timer, preventing the system from being reset.
// This method must be called periodically (before the timeout expires)
// to keep the system running. The frequency depends on the watchdog's
// configured timeout value.
//
// This method includes retry logic for transient errors, as watchdog petting
// is critical for system stability.
//
// Returns:
//   - error: An error if the watchdog cannot be pet after retries, or if the device is not open
//
// This method uses the WDIOC_KEEPALIVE ioctl command to communicate with
// the kernel watchdog driver. If the ioctl fails consistently, the watchdog timer
// continues counting down and may reset the system.
func (w *Watchdog) Pet() error {
	if !w.isOpen {
		return fmt.Errorf("watchdog device is not open")
	}

	if w.file == nil {
		return fmt.Errorf("watchdog file descriptor is nil")
	}

	// Retry watchdog pet operations for transient errors
	ctx := context.Background()
	err := retry.Do(ctx, w.retryConfig, "pet watchdog", func() error {
		// Use WDIOC_KEEPALIVE ioctl to reset the watchdog timer
		// The third parameter (0) is ignored for WDIOC_KEEPALIVE
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(w.file.Fd()), WDIOC_KEEPALIVE, 0)
		if errno != 0 {
			// Convert syscall error to Go error and determine if it's retryable
			syscallErr := fmt.Errorf("ioctl WDIOC_KEEPALIVE failed: %w", errno)
			return retry.NewRetryableError(syscallErr, retry.IsTransientError(errno), "pet watchdog")
		}
		return nil
	})

	if err != nil {
		w.logger.Error(err, "Failed to pet watchdog after retries")
		return fmt.Errorf("failed to pet watchdog at %s: %w", w.path, err)
	}

	w.logger.V(2).Info("Watchdog pet successful")
	return nil
}

// Close closes the watchdog device file descriptor and releases associated resources.
//
// IMPORTANT: Closing the watchdog device may have different behaviors depending
// on the specific watchdog driver:
// - Some drivers stop the watchdog timer when the device is closed
// - Others continue running and will reset the system if not reopened and pet
// - Some require writing 'V' to the device before closing to stop the timer
//
// Returns:
//   - error: An error if the device cannot be closed properly
//
// This method marks the watchdog as closed and prevents further operations.
// It's safe to call Close() multiple times.
func (w *Watchdog) Close() error {
	if !w.isOpen {
		return nil // Already closed, not an error
	}

	if w.file == nil {
		w.isOpen = false
		return nil
	}

	// Some watchdog devices require writing 'V' to stop the timer before closing
	// This is known as the "magic close" feature and prevents accidental system resets
	// We'll write 'V' to attempt graceful shutdown, but don't fail if it doesn't work
	_, _ = w.file.Write([]byte("V"))

	err := w.file.Close()
	w.isOpen = false
	w.file = nil

	if err != nil {
		return fmt.Errorf("failed to close watchdog device at %s: %w", w.path, err)
	}

	w.logger.V(1).Info("Watchdog device closed")
	return nil
}

// IsOpen returns true if the watchdog device is currently open and available for operations.
func (w *Watchdog) IsOpen() bool {
	return w.isOpen
}

// Path returns the filesystem path of the watchdog device.
func (w *Watchdog) Path() string {
	return w.path
}
