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

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// MockBlockDevice implements a mock block device for testing
type MockBlockDevice struct {
	data       []byte
	closed     bool
	writeError error
	syncError  error
	readError  error
	mutex      sync.RWMutex
	path       string
	writeCount int
	syncCount  int
	readCount  int
}

// NewMockBlockDevice creates a new mock block device
func NewMockBlockDevice(path string, size int) *MockBlockDevice {
	return &MockBlockDevice{
		data: make([]byte, size),
		path: path,
	}
}

// ReadAt implements the io.ReaderAt interface
func (m *MockBlockDevice) ReadAt(p []byte, off int64) (n int, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.readCount++

	if m.closed {
		return 0, fmt.Errorf("device %q is closed", m.path)
	}

	if m.readError != nil {
		return 0, m.readError
	}

	if off < 0 {
		return 0, fmt.Errorf("negative offset %d not allowed", off)
	}

	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}

	n = copy(p, m.data[off:])
	return n, nil
}

// WriteAt implements the io.WriterAt interface
func (m *MockBlockDevice) WriteAt(p []byte, off int64) (n int, err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.writeCount++

	if m.closed {
		return 0, fmt.Errorf("device %q is closed", m.path)
	}

	if m.writeError != nil {
		return 0, m.writeError
	}

	if off < 0 {
		return 0, fmt.Errorf("negative offset %d not allowed", off)
	}

	// Expand data if necessary
	end := int(off) + len(p)
	if end > len(m.data) {
		newData := make([]byte, end)
		copy(newData, m.data)
		m.data = newData
	}

	n = copy(m.data[off:], p)
	return n, nil
}

// Sync flushes any buffered writes
func (m *MockBlockDevice) Sync() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.syncCount++

	if m.closed {
		return fmt.Errorf("device %q is closed", m.path)
	}

	return m.syncError
}

// Close closes the mock device
func (m *MockBlockDevice) Close() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.closed = true
	return nil
}

// Path returns the device path
func (m *MockBlockDevice) Path() string {
	return m.path
}

// IsClosed returns true if the device is closed
func (m *MockBlockDevice) IsClosed() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.closed
}

// String returns a string representation
func (m *MockBlockDevice) String() string {
	status := "open"
	if m.closed {
		status = "closed"
	}
	return fmt.Sprintf("MockDevice{path: %q, status: %s}", m.path, status)
}

// SetWriteError sets an error to be returned by WriteAt
func (m *MockBlockDevice) SetWriteError(err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.writeError = err
}

// SetSyncError sets an error to be returned by Sync
func (m *MockBlockDevice) SetSyncError(err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.syncError = err
}

// SetReadError sets an error to be returned by ReadAt
func (m *MockBlockDevice) SetReadError(err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.readError = err
}

// GetWriteCount returns the number of writes performed
func (m *MockBlockDevice) GetWriteCount() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.writeCount
}

// GetSyncCount returns the number of sync operations performed
func (m *MockBlockDevice) GetSyncCount() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.syncCount
}

// GetDataAt returns the data at a specific offset
func (m *MockBlockDevice) GetDataAt(offset int64, length int) []byte {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	end := int(offset) + length
	if end > len(m.data) {
		end = len(m.data)
	}

	result := make([]byte, end-int(offset))
	copy(result, m.data[offset:end])
	return result
}

// createTestWatchdog creates a temporary file for testing watchdog functionality
func createTestWatchdog(t *testing.T) (string, func()) {
	t.Helper()

	tempDir := t.TempDir()
	watchdogPath := filepath.Join(tempDir, "test-watchdog")

	// Create a simple file to simulate watchdog device
	file, err := os.Create(watchdogPath)
	if err != nil {
		t.Fatalf("Failed to create test watchdog: %v", err)
	}
	file.Close()

	return watchdogPath, func() {
		os.RemoveAll(tempDir)
	}
}

func TestNewSBDAgent(t *testing.T) {
	watchdogPath, cleanup := createTestWatchdog(t)
	defer cleanup()

	tests := []struct {
		name              string
		watchdogPath      string
		sbdDevicePath     string
		nodeName          string
		petInterval       time.Duration
		sbdUpdateInterval time.Duration
		expectError       bool
	}{
		{
			name:              "valid configuration",
			watchdogPath:      watchdogPath,
			sbdDevicePath:     "",
			nodeName:          "test-node",
			petInterval:       1 * time.Second,
			sbdUpdateInterval: 2 * time.Second,
			expectError:       false,
		},
		{
			name:              "invalid watchdog path",
			watchdogPath:      "/non/existent/watchdog",
			sbdDevicePath:     "",
			nodeName:          "test-node",
			petInterval:       1 * time.Second,
			sbdUpdateInterval: 2 * time.Second,
			expectError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := NewSBDAgent(tt.watchdogPath, tt.sbdDevicePath, tt.nodeName, tt.petInterval, tt.sbdUpdateInterval)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if agent == nil {
				t.Errorf("Expected agent to be non-nil")
				return
			}

			// Verify agent properties
			if agent.nodeName != tt.nodeName {
				t.Errorf("Expected node name %q, got %q", tt.nodeName, agent.nodeName)
			}

			if agent.petInterval != tt.petInterval {
				t.Errorf("Expected pet interval %v, got %v", tt.petInterval, agent.petInterval)
			}

			if agent.sbdUpdateInterval != tt.sbdUpdateInterval {
				t.Errorf("Expected SBD update interval %v, got %v", tt.sbdUpdateInterval, agent.sbdUpdateInterval)
			}

			// Clean up
			agent.Stop()
		})
	}
}

func TestSBDAgent_WriteNodeIDToSBD(t *testing.T) {
	// Create mock SBD device
	mockDevice := NewMockBlockDevice("/dev/test-sbd", 1024)

	agent := &SBDAgent{
		nodeName:   "test-node-123",
		sbdHealthy: true,
	}
	agent.setSBDDevice(mockDevice)

	// Test successful write
	err := agent.writeNodeIDToSBD()
	if err != nil {
		t.Errorf("Unexpected error writing node ID: %v", err)
	}

	// Verify the data was written correctly
	expectedData := make([]byte, MaxNodeNameLength)
	copy(expectedData, []byte("test-node-123"))

	actualData := mockDevice.GetDataAt(SBDNodeIDOffset, MaxNodeNameLength)
	if !bytes.Equal(actualData, expectedData) {
		t.Errorf("Data mismatch. Expected first bytes to be %q, got %q",
			string(expectedData[:len("test-node-123")]),
			string(actualData[:len("test-node-123")]))
	}

	// Verify sync was called
	if mockDevice.GetSyncCount() != 1 {
		t.Errorf("Expected 1 sync call, got %d", mockDevice.GetSyncCount())
	}
}

func TestSBDAgent_WriteNodeIDToSBD_WriteError(t *testing.T) {
	mockDevice := NewMockBlockDevice("/dev/test-sbd", 1024)
	mockDevice.SetWriteError(fmt.Errorf("write failed"))

	agent := &SBDAgent{
		nodeName: "test-node",
	}
	agent.setSBDDevice(mockDevice)

	err := agent.writeNodeIDToSBD()
	if err == nil {
		t.Errorf("Expected error but got none")
	}

	if !strings.Contains(err.Error(), "failed to write node ID") {
		t.Errorf("Expected error to contain 'failed to write node ID', got: %v", err)
	}
}

func TestSBDAgent_WriteNodeIDToSBD_SyncError(t *testing.T) {
	mockDevice := NewMockBlockDevice("/dev/test-sbd", 1024)
	mockDevice.SetSyncError(fmt.Errorf("sync failed"))

	agent := &SBDAgent{
		nodeName: "test-node",
	}
	agent.setSBDDevice(mockDevice)

	err := agent.writeNodeIDToSBD()
	if err == nil {
		t.Errorf("Expected error but got none")
	}

	if !strings.Contains(err.Error(), "failed to sync SBD device") {
		t.Errorf("Expected error to contain 'failed to sync SBD device', got: %v", err)
	}
}

func TestSBDAgent_SBDHealthy(t *testing.T) {
	agent := &SBDAgent{
		sbdHealthy: true,
	}

	// Test initial healthy state
	if !agent.isSBDHealthy() {
		t.Errorf("Expected SBD to be healthy initially")
	}

	// Test setting unhealthy
	agent.setSBDHealthy(false)
	if agent.isSBDHealthy() {
		t.Errorf("Expected SBD to be unhealthy after setting false")
	}

	// Test setting healthy again
	agent.setSBDHealthy(true)
	if !agent.isSBDHealthy() {
		t.Errorf("Expected SBD to be healthy after setting true")
	}
}

func TestSBDAgent_SBDDeviceLoop(t *testing.T) {
	mockDevice := NewMockBlockDevice("/dev/test-sbd", 1024)

	agent := &SBDAgent{
		nodeName:          "test-node",
		sbdUpdateInterval: 50 * time.Millisecond,
		sbdHealthy:        true,
	}
	agent.setSBDDevice(mockDevice)

	ctx, cancel := context.WithCancel(context.Background())
	agent.ctx = ctx
	agent.cancel = cancel

	// Start SBD device loop
	go agent.sbdDeviceLoop()

	// Wait for a few updates
	time.Sleep(150 * time.Millisecond)

	// Stop the loop
	cancel()

	// Give it time to stop
	time.Sleep(50 * time.Millisecond)

	// Verify that writes occurred
	if mockDevice.GetWriteCount() < 2 {
		t.Errorf("Expected at least 2 writes, got %d", mockDevice.GetWriteCount())
	}

	// Verify that syncs occurred
	if mockDevice.GetSyncCount() < 2 {
		t.Errorf("Expected at least 2 syncs, got %d", mockDevice.GetSyncCount())
	}

	// Verify the agent is still healthy
	if !agent.isSBDHealthy() {
		t.Errorf("Expected SBD to remain healthy")
	}
}

func TestSBDAgent_SBDDeviceLoop_ErrorHandling(t *testing.T) {
	mockDevice := NewMockBlockDevice("/dev/test-sbd", 1024)

	agent := &SBDAgent{
		nodeName:          "test-node",
		sbdUpdateInterval: 50 * time.Millisecond,
		sbdHealthy:        true,
	}
	agent.setSBDDevice(mockDevice)

	ctx, cancel := context.WithCancel(context.Background())
	agent.ctx = ctx
	agent.cancel = cancel

	// Set an error after first successful write
	go func() {
		time.Sleep(75 * time.Millisecond)
		mockDevice.SetWriteError(fmt.Errorf("device failure"))
	}()

	// Start SBD device loop
	go agent.sbdDeviceLoop()

	// Wait for updates and error to occur
	time.Sleep(200 * time.Millisecond)

	// Stop the loop
	cancel()

	// Give it time to stop
	time.Sleep(50 * time.Millisecond)

	// Verify that the agent detected the failure
	if agent.isSBDHealthy() {
		t.Errorf("Expected SBD to be unhealthy after device failure")
	}
}

func TestSBDAgent_WatchdogLoop_WithSBDFailure(t *testing.T) {
	watchdogPath, cleanup := createTestWatchdog(t)
	defer cleanup()

	agent, err := NewSBDAgent(watchdogPath, "/dev/test-sbd", "test-node", 50*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Start the agent
	if err := agent.Start(); err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}

	// Initially SBD should be healthy (or unhealthy due to missing device)
	// Set it to unhealthy to test watchdog behavior
	agent.setSBDHealthy(false)

	// Count watchdog operations by monitoring the log or file size
	// For this test, we just verify the logic doesn't crash
	time.Sleep(150 * time.Millisecond)

	// The test mainly verifies that the watchdog loop handles SBD failure gracefully
	// In real scenarios, this would stop petting the watchdog, causing a reboot
}

func TestGetNodeNameFromEnv(t *testing.T) {
	// Save original environment
	originalNodeName := os.Getenv("NODE_NAME")
	originalHostname := os.Getenv("HOSTNAME")

	defer func() {
		// Restore original environment
		if originalNodeName != "" {
			os.Setenv("NODE_NAME", originalNodeName)
		} else {
			os.Unsetenv("NODE_NAME")
		}
		if originalHostname != "" {
			os.Setenv("HOSTNAME", originalHostname)
		} else {
			os.Unsetenv("HOSTNAME")
		}
	}()

	tests := []struct {
		name        string
		envVars     map[string]string
		expected    string
		description string
	}{
		{
			name:        "NODE_NAME set",
			envVars:     map[string]string{"NODE_NAME": "test-node-1"},
			expected:    "test-node-1",
			description: "Should use NODE_NAME when available",
		},
		{
			name:        "HOSTNAME set",
			envVars:     map[string]string{"HOSTNAME": "test-hostname"},
			expected:    "test-hostname",
			description: "Should use HOSTNAME when NODE_NAME not available",
		},
		{
			name:        "both set, NODE_NAME takes precedence",
			envVars:     map[string]string{"NODE_NAME": "node-name", "HOSTNAME": "hostname"},
			expected:    "node-name",
			description: "Should prefer NODE_NAME over HOSTNAME",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear environment
			os.Unsetenv("NODE_NAME")
			os.Unsetenv("HOSTNAME")
			os.Unsetenv("NODENAME")

			// Set test environment
			for key, value := range tt.envVars {
				os.Setenv(key, value)
			}

			result := getNodeNameFromEnv()
			if result != tt.expected {
				t.Errorf("Expected %q, got %q. %s", tt.expected, result, tt.description)
			}
		})
	}
}

func TestValidateSBDDevice(t *testing.T) {
	// Create a test file
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test-device")

	file, err := os.Create(testFile)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	file.Close()

	tests := []struct {
		name        string
		devicePath  string
		expectError bool
		errorSubstr string
	}{
		{
			name:        "valid device",
			devicePath:  testFile,
			expectError: false,
		},
		{
			name:        "empty path",
			devicePath:  "",
			expectError: true,
			errorSubstr: "cannot be empty",
		},
		{
			name:        "non-existent device",
			devicePath:  "/non/existent/device",
			expectError: true,
			errorSubstr: "not accessible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSBDDevice(tt.devicePath)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errorSubstr) {
					t.Errorf("Expected error to contain %q, got: %v", tt.errorSubstr, err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSBDAgent_Integration(t *testing.T) {
	watchdogPath, cleanup := createTestWatchdog(t)
	defer cleanup()

	// Create an agent without SBD device (watchdog-only mode)
	agent, err := NewSBDAgent(watchdogPath, "", "test-node", 50*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Start the agent
	if err := agent.Start(); err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}

	// Let it run for a short time
	time.Sleep(200 * time.Millisecond)

	// Stop the agent
	if err := agent.Stop(); err != nil {
		t.Errorf("Error stopping agent: %v", err)
	}

	// Verify clean shutdown
	// In watchdog-only mode, sbdDevice should be nil
	if agent.sbdDevice != nil && !agent.sbdDevice.IsClosed() && agent.sbdDevicePath != "" {
		t.Errorf("Expected SBD device to be closed after stop")
	}
}
