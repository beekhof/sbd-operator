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

// Package blockdevice provides utilities for interacting with raw block devices.
// This package is designed specifically for SBD (Storage-Based Death) operations
// that require direct, synchronous access to block devices for reliable fencing.
package blockdevice

import (
	"fmt"
	"io"
	"os"
)

// Device represents a raw block device that can be read from and written to.
// It implements the io.ReaderAt and io.WriterAt interfaces for positioned I/O operations.
// All operations are performed synchronously to ensure data integrity for SBD operations.
type Device struct {
	// file is the underlying file handle to the block device
	file *os.File
	// path is the filesystem path to the block device
	path string
}

// Open opens a raw block device at the specified path for read/write operations.
// The device is opened with O_RDWR and O_SYNC flags to ensure synchronous I/O,
// which is critical for SBD operations where data must be immediately written to disk.
//
// Parameters:
//   - path: The filesystem path to the block device (e.g., "/dev/sdb1")
//
// Returns:
//   - *Device: A new Device instance if successful
//   - error: An error if the device cannot be opened
//
// Example:
//
//	device, err := blockdevice.Open("/dev/sdb1")
//	if err != nil {
//	    log.Fatalf("Failed to open device: %v", err)
//	}
//	defer device.Close()
func Open(path string) (*Device, error) {
	if path == "" {
		return nil, fmt.Errorf("device path cannot be empty")
	}

	// Open the device with read/write access and synchronous I/O
	// O_SYNC ensures that all writes are immediately flushed to disk
	file, err := os.OpenFile(path, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open block device %q: %w", path, err)
	}

	return &Device{
		file: file,
		path: path,
	}, nil
}

// ReadAt reads len(p) bytes from the device starting at byte offset off.
// It implements the io.ReaderAt interface, allowing for positioned reads
// without affecting the device's current file position.
//
// This method is safe for concurrent use as it doesn't modify any shared state
// and uses the positioned read system call.
//
// Parameters:
//   - p: The buffer to read data into
//   - off: The byte offset from the beginning of the device to start reading
//
// Returns:
//   - n: The number of bytes actually read
//   - err: An error if the read operation fails
//
// The method returns an error if:
//   - The device has been closed
//   - The offset is negative
//   - A system-level read error occurs
func (d *Device) ReadAt(p []byte, off int64) (n int, err error) {
	if d.file == nil {
		return 0, fmt.Errorf("device %q is closed", d.path)
	}

	if off < 0 {
		return 0, fmt.Errorf("negative offset %d not allowed", off)
	}

	n, err = d.file.ReadAt(p, off)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("failed to read from device %q at offset %d: %w", d.path, off, err)
	}

	return n, err
}

// WriteAt writes len(p) bytes to the device starting at byte offset off.
// It implements the io.WriterAt interface, allowing for positioned writes
// without affecting the device's current file position.
//
// Since the device is opened with O_SYNC, this method ensures that data
// is immediately written to the underlying storage before returning.
//
// This method is safe for concurrent use as it doesn't modify any shared state
// and uses the positioned write system call.
//
// Parameters:
//   - p: The data to write to the device
//   - off: The byte offset from the beginning of the device to start writing
//
// Returns:
//   - n: The number of bytes actually written
//   - err: An error if the write operation fails
//
// The method returns an error if:
//   - The device has been closed
//   - The offset is negative
//   - A system-level write error occurs
//   - Not all bytes could be written
func (d *Device) WriteAt(p []byte, off int64) (n int, err error) {
	if d.file == nil {
		return 0, fmt.Errorf("device %q is closed", d.path)
	}

	if off < 0 {
		return 0, fmt.Errorf("negative offset %d not allowed", off)
	}

	n, err = d.file.WriteAt(p, off)
	if err != nil {
		return n, fmt.Errorf("failed to write to device %q at offset %d: %w", d.path, off, err)
	}

	if n != len(p) {
		return n, fmt.Errorf("partial write to device %q: wrote %d bytes, expected %d", d.path, n, len(p))
	}

	return n, nil
}

// Sync flushes any buffered writes to the underlying storage device.
// This method explicitly calls the system sync operation to ensure all
// pending writes are committed to disk before returning.
//
// While the device is opened with O_SYNC flag (meaning writes should be
// synchronous), calling Sync() provides an additional guarantee that all
// data has been written to persistent storage. This is particularly important
// for SBD operations where data integrity is critical.
//
// Returns:
//   - error: An error if the sync operation fails
//
// The method returns an error if:
//   - The device has been closed
//   - A system-level sync error occurs
func (d *Device) Sync() error {
	if d.file == nil {
		return fmt.Errorf("device %q is closed", d.path)
	}

	if err := d.file.Sync(); err != nil {
		return fmt.Errorf("failed to sync device %q: %w", d.path, err)
	}

	return nil
}

// Close closes the block device and releases any associated resources.
// After calling Close(), the Device instance should not be used for any
// further operations.
//
// It is safe to call Close() multiple times; subsequent calls will have no effect.
//
// Returns:
//   - error: An error if the close operation fails
//
// Example:
//
//	device, err := blockdevice.Open("/dev/sdb1")
//	if err != nil {
//	    log.Fatalf("Failed to open device: %v", err)
//	}
//	defer device.Close()
//
//	// Use device...
//
//	if err := device.Close(); err != nil {
//	    log.Printf("Warning: failed to close device: %v", err)
//	}
func (d *Device) Close() error {
	if d.file == nil {
		// Already closed, no-op
		return nil
	}

	err := d.file.Close()
	d.file = nil // Mark as closed

	if err != nil {
		return fmt.Errorf("failed to close device %q: %w", d.path, err)
	}

	return nil
}

// String returns a string representation of the Device for debugging purposes.
func (d *Device) String() string {
	if d.file == nil {
		return fmt.Sprintf("Device{path: %q, status: closed}", d.path)
	}
	return fmt.Sprintf("Device{path: %q, status: open}", d.path)
}

// Path returns the filesystem path of the block device.
func (d *Device) Path() string {
	return d.path
}

// IsClosed returns true if the device has been closed.
func (d *Device) IsClosed() bool {
	return d.file == nil
}
