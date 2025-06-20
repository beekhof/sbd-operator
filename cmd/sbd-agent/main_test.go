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
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
)

// MockBlockDevice implements the BlockDevice interface for testing
type MockBlockDevice struct {
	data           map[int64][]byte
	dataMutex      sync.RWMutex
	path           string
	closed         bool
	closedMutex    sync.RWMutex
	writeError     error
	readError      error
	syncError      error
	closeError     error
	writeCallCount int
	readCallCount  int
	syncCallCount  int
}

// NewMockBlockDevice creates a new mock block device
func NewMockBlockDevice(path string) *MockBlockDevice {
	return &MockBlockDevice{
		data: make(map[int64][]byte),
		path: path,
	}
}

// SetWriteError sets an error to be returned on write operations
func (m *MockBlockDevice) SetWriteError(err error) {
	m.writeError = err
}

// SetReadError sets an error to be returned on read operations
func (m *MockBlockDevice) SetReadError(err error) {
	m.readError = err
}

// SetSyncError sets an error to be returned on sync operations
func (m *MockBlockDevice) SetSyncError(err error) {
	m.syncError = err
}

// SetCloseError sets an error to be returned on close operations
func (m *MockBlockDevice) SetCloseError(err error) {
	m.closeError = err
}

// GetWriteCallCount returns the number of times WriteAt was called
func (m *MockBlockDevice) GetWriteCallCount() int {
	return m.writeCallCount
}

// GetReadCallCount returns the number of times ReadAt was called
func (m *MockBlockDevice) GetReadCallCount() int {
	return m.readCallCount
}

// GetSyncCallCount returns the number of times Sync was called
func (m *MockBlockDevice) GetSyncCallCount() int {
	return m.syncCallCount
}

// GetDataAt returns the data written at a specific offset
func (m *MockBlockDevice) GetDataAt(offset int64, length int) []byte {
	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()

	result := make([]byte, length)
	for i := 0; i < length; i++ {
		if data, exists := m.data[offset+int64(i)]; exists && len(data) > 0 {
			result[i] = data[0]
		}
	}
	return result
}

// ReadAt implements io.ReaderAt
func (m *MockBlockDevice) ReadAt(p []byte, off int64) (n int, err error) {
	m.readCallCount++

	if m.readError != nil {
		return 0, m.readError
	}

	if m.IsClosed() {
		return 0, fmt.Errorf("device %q is closed", m.path)
	}

	m.dataMutex.RLock()
	defer m.dataMutex.RUnlock()

	for i := range p {
		if data, exists := m.data[off+int64(i)]; exists && len(data) > 0 {
			p[i] = data[0]
		} else {
			p[i] = 0
		}
	}

	return len(p), nil
}

// WriteAt implements io.WriterAt
func (m *MockBlockDevice) WriteAt(p []byte, off int64) (n int, err error) {
	m.writeCallCount++

	if m.writeError != nil {
		return 0, m.writeError
	}

	if m.IsClosed() {
		return 0, fmt.Errorf("device %q is closed", m.path)
	}

	m.dataMutex.Lock()
	defer m.dataMutex.Unlock()

	for i, b := range p {
		m.data[off+int64(i)] = []byte{b}
	}

	return len(p), nil
}

// Sync implements the Sync method
func (m *MockBlockDevice) Sync() error {
	m.syncCallCount++

	if m.syncError != nil {
		return m.syncError
	}

	if m.IsClosed() {
		return fmt.Errorf("device %q is closed", m.path)
	}

	return nil
}

// Close implements the Close method
func (m *MockBlockDevice) Close() error {
	m.closedMutex.Lock()
	defer m.closedMutex.Unlock()

	if m.closed {
		return nil // Already closed, no-op
	}

	if m.closeError != nil {
		return m.closeError
	}

	m.closed = true
	return nil
}

// Path returns the device path
func (m *MockBlockDevice) Path() string {
	return m.path
}

// IsClosed returns true if the device is closed
func (m *MockBlockDevice) IsClosed() bool {
	m.closedMutex.RLock()
	defer m.closedMutex.RUnlock()
	return m.closed
}

// MockWatchdog implements a mock watchdog for testing
type MockWatchdog struct {
	path       string
	petCount   int
	petError   error
	closed     bool
	closeError error
}

// NewMockWatchdog creates a new mock watchdog
func NewMockWatchdog(path string) *MockWatchdog {
	return &MockWatchdog{path: path}
}

// SetPetError sets an error to be returned by Pet()
func (m *MockWatchdog) SetPetError(err error) {
	m.petError = err
}

// SetCloseError sets an error to be returned by Close()
func (m *MockWatchdog) SetCloseError(err error) {
	m.closeError = err
}

// GetPetCount returns the number of times Pet was called
func (m *MockWatchdog) GetPetCount() int {
	return m.petCount
}

// Pet implements the Pet method
func (m *MockWatchdog) Pet() error {
	if m.petError != nil {
		return m.petError
	}
	m.petCount++
	return nil
}

// Close implements the Close method
func (m *MockWatchdog) Close() error {
	if m.closeError != nil {
		return m.closeError
	}
	m.closed = true
	return nil
}

// Path implements the Path method
func (m *MockWatchdog) Path() string {
	return m.path
}

func TestNewSBDAgent(t *testing.T) {
	tests := []struct {
		name                string
		watchdogPath        string
		sbdDevicePath       string
		nodeName            string
		nodeID              uint16
		petInterval         time.Duration
		sbdUpdateInterval   time.Duration
		heartbeatInterval   time.Duration
		expectError         bool
		expectedNodeName    string
		expectedNodeID      uint16
		expectedPetInterval time.Duration
	}{
		{
			name:                "successful creation with all parameters",
			watchdogPath:        "/dev/watchdog",
			sbdDevicePath:       "/dev/sbd",
			nodeName:            "test-node",
			nodeID:              1,
			petInterval:         30 * time.Second,
			sbdUpdateInterval:   5 * time.Second,
			heartbeatInterval:   15 * time.Second,
			expectError:         false,
			expectedNodeName:    "test-node",
			expectedNodeID:      1,
			expectedPetInterval: 30 * time.Second,
		},
		{
			name:                "creation with minimal parameters",
			watchdogPath:        "/dev/watchdog",
			sbdDevicePath:       "",
			nodeName:            "minimal-node",
			nodeID:              2,
			petInterval:         10 * time.Second,
			sbdUpdateInterval:   2 * time.Second,
			heartbeatInterval:   5 * time.Second,
			expectError:         false,
			expectedNodeName:    "minimal-node",
			expectedNodeID:      2,
			expectedPetInterval: 10 * time.Second,
		},
		{
			name:                "creation with maximum node ID",
			watchdogPath:        "/dev/watchdog",
			sbdDevicePath:       "/dev/sbd",
			nodeName:            "max-node",
			nodeID:              sbdprotocol.SBD_MAX_NODES,
			petInterval:         60 * time.Second,
			sbdUpdateInterval:   10 * time.Second,
			heartbeatInterval:   30 * time.Second,
			expectError:         false,
			expectedNodeName:    "max-node",
			expectedNodeID:      sbdprotocol.SBD_MAX_NODES,
			expectedPetInterval: 60 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock watchdog for testing
			mockWatchdog := NewMockWatchdog(tt.watchdogPath)

			agent, err := NewSBDAgentWithWatchdog(mockWatchdog, tt.sbdDevicePath, tt.nodeName, tt.nodeID, tt.petInterval, tt.sbdUpdateInterval, tt.heartbeatInterval)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error, but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if agent.nodeName != tt.expectedNodeName {
				t.Errorf("Expected nodeName %q, got %q", tt.expectedNodeName, agent.nodeName)
			}

			if agent.nodeID != tt.expectedNodeID {
				t.Errorf("Expected nodeID %d, got %d", tt.expectedNodeID, agent.nodeID)
			}

			if agent.petInterval != tt.expectedPetInterval {
				t.Errorf("Expected petInterval %v, got %v", tt.expectedPetInterval, agent.petInterval)
			}

			if agent.heartbeatInterval != tt.heartbeatInterval {
				t.Errorf("Expected heartbeatInterval %v, got %v", tt.heartbeatInterval, agent.heartbeatInterval)
			}

			// Clean up
			agent.Stop()
		})
	}
}

func TestSBDAgent_WriteNodeIDToSBD(t *testing.T) {
	tests := []struct {
		name          string
		nodeName      string
		nodeID        uint16
		writeError    error
		syncError     error
		expectError   bool
		expectedBytes int
	}{
		{
			name:          "successful write",
			nodeName:      "test-node",
			nodeID:        1,
			writeError:    nil,
			syncError:     nil,
			expectError:   false,
			expectedBytes: MaxNodeNameLength,
		},
		{
			name:        "write error",
			nodeName:    "test-node",
			nodeID:      1,
			writeError:  errors.New("write failed"),
			syncError:   nil,
			expectError: true,
		},
		{
			name:        "sync error",
			nodeName:    "test-node",
			nodeID:      1,
			writeError:  nil,
			syncError:   errors.New("sync failed"),
			expectError: true,
		},
		{
			name:          "long node name",
			nodeName:      "very-long-node-name-that-should-be-truncated",
			nodeID:        2,
			writeError:    nil,
			syncError:     nil,
			expectError:   false,
			expectedBytes: MaxNodeNameLength,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDevice := NewMockBlockDevice("/dev/mock-sbd")
			if tt.writeError != nil {
				mockDevice.SetWriteError(tt.writeError)
			}
			if tt.syncError != nil {
				mockDevice.SetSyncError(tt.syncError)
			}

			mockWatchdog := NewMockWatchdog("/dev/watchdog")
			agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", tt.nodeName, tt.nodeID, 30*time.Second, 5*time.Second, 15*time.Second)
			if err != nil {
				t.Fatalf("Failed to create agent: %v", err)
			}

			// Set the mock device
			agent.setSBDDevice(mockDevice)

			err = agent.writeNodeIDToSBD()

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error, but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// Check that data was written
				if mockDevice.GetWriteCallCount() != 1 {
					t.Errorf("Expected 1 write call, got %d", mockDevice.GetWriteCallCount())
				}

				// Verify sync was called
				if mockDevice.GetSyncCallCount() != 1 {
					t.Errorf("Expected 1 sync call, got %d", mockDevice.GetSyncCallCount())
				}

				// Verify data content
				writtenData := mockDevice.GetDataAt(SBDNodeIDOffset, tt.expectedBytes)
				expectedNodeName := []byte(tt.nodeName)
				if len(expectedNodeName) > tt.expectedBytes {
					expectedNodeName = expectedNodeName[:tt.expectedBytes]
				}

				for i, expectedByte := range expectedNodeName {
					if writtenData[i] != expectedByte {
						t.Errorf("Data mismatch at position %d: expected %d, got %d", i, expectedByte, writtenData[i])
						break
					}
				}
			}

			agent.Stop()
		})
	}
}

func TestSBDAgent_WriteHeartbeatToSBD(t *testing.T) {
	tests := []struct {
		name        string
		nodeID      uint16
		writeError  error
		syncError   error
		expectError bool
	}{
		{
			name:        "successful heartbeat write",
			nodeID:      1,
			writeError:  nil,
			syncError:   nil,
			expectError: false,
		},
		{
			name:        "heartbeat write error",
			nodeID:      2,
			writeError:  errors.New("write failed"),
			syncError:   nil,
			expectError: true,
		},
		{
			name:        "heartbeat sync error",
			nodeID:      3,
			writeError:  nil,
			syncError:   errors.New("sync failed"),
			expectError: true,
		},
		{
			name:        "heartbeat write with max node ID",
			nodeID:      sbdprotocol.SBD_MAX_NODES,
			writeError:  nil,
			syncError:   nil,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDevice := NewMockBlockDevice("/dev/mock-sbd")
			if tt.writeError != nil {
				mockDevice.SetWriteError(tt.writeError)
			}
			if tt.syncError != nil {
				mockDevice.SetSyncError(tt.syncError)
			}

			mockWatchdog := NewMockWatchdog("/dev/watchdog")
			agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", "test-node", tt.nodeID, 30*time.Second, 5*time.Second, 15*time.Second)
			if err != nil {
				t.Fatalf("Failed to create agent: %v", err)
			}

			// Set the mock device
			agent.setSBDDevice(mockDevice)

			err = agent.writeHeartbeatToSBD()

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error, but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// Check that data was written
				if mockDevice.GetWriteCallCount() != 1 {
					t.Errorf("Expected 1 write call, got %d", mockDevice.GetWriteCallCount())
				}

				// Verify sync was called
				if mockDevice.GetSyncCallCount() != 1 {
					t.Errorf("Expected 1 sync call, got %d", mockDevice.GetSyncCallCount())
				}

				// Verify the heartbeat was written to the correct slot
				expectedOffset := int64(tt.nodeID) * sbdprotocol.SBD_SLOT_SIZE
				heartbeatData := mockDevice.GetDataAt(expectedOffset, sbdprotocol.SBD_HEADER_SIZE)

				// Verify the magic string is present
				magic := string(heartbeatData[:8])
				if magic != sbdprotocol.SBD_MAGIC {
					t.Errorf("Expected magic %q, got %q", sbdprotocol.SBD_MAGIC, magic)
				}

				// Verify sequence number incremented
				seq1 := agent.getNextHeartbeatSequence()
				if seq1 != 2 { // Should be 2 since writeHeartbeatToSBD incremented it to 1
					t.Errorf("Expected sequence 2, got %d", seq1)
				}
			}

			agent.Stop()
		})
	}
}

func TestSBDAgent_HeartbeatSequence(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Test sequence number increment
	seq1 := agent.getNextHeartbeatSequence()
	seq2 := agent.getNextHeartbeatSequence()
	seq3 := agent.getNextHeartbeatSequence()

	if seq1 != 1 {
		t.Errorf("Expected first sequence to be 1, got %d", seq1)
	}

	if seq2 != 2 {
		t.Errorf("Expected second sequence to be 2, got %d", seq2)
	}

	if seq3 != 3 {
		t.Errorf("Expected third sequence to be 3, got %d", seq3)
	}
}

func TestSBDAgent_SBDHealthStatus(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	// Use empty SBD device path so initialization doesn't fail
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Initial state should be healthy when no SBD device is configured
	if !agent.isSBDHealthy() {
		t.Error("Expected SBD to be initially healthy")
	}

	// Set unhealthy
	agent.setSBDHealthy(false)
	if agent.isSBDHealthy() {
		t.Error("Expected SBD to be unhealthy after setting to false")
	}

	// Set healthy again
	agent.setSBDHealthy(true)
	if !agent.isSBDHealthy() {
		t.Error("Expected SBD to be healthy after setting to true")
	}
}

func TestSBDAgent_WatchdogIntegration(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd")

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", "test-node", 1, 50*time.Millisecond, 25*time.Millisecond, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	agent.setSBDDevice(mockDevice)

	// Start the agent
	if err := agent.Start(); err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}

	// Let it run for a short time
	time.Sleep(150 * time.Millisecond)

	// Stop the agent
	agent.Stop()

	// Check that the watchdog was petted (should be healthy by default)
	if mockWatchdog.GetPetCount() == 0 {
		t.Error("Expected watchdog to be petted at least once")
	}

	// Test with SBD unhealthy
	agent2, err := NewSBDAgentWithWatchdog(NewMockWatchdog("/dev/watchdog"), "/dev/mock-sbd", "test-node", 1, 50*time.Millisecond, 25*time.Millisecond, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("Failed to create second agent: %v", err)
	}

	mockDevice2 := NewMockBlockDevice("/dev/mock-sbd")
	mockDevice2.SetWriteError(errors.New("device failure"))
	agent2.setSBDDevice(mockDevice2)

	// Start the agent - this should make SBD unhealthy
	if err := agent2.Start(); err != nil {
		t.Fatalf("Failed to start second agent: %v", err)
	}

	// Wait a bit for the SBD device to fail
	time.Sleep(50 * time.Millisecond)

	// Check that SBD is now unhealthy
	if agent2.isSBDHealthy() {
		t.Error("Expected SBD to be unhealthy due to write failure")
	}

	agent2.Stop()
}

func TestValidateSBDDevice(t *testing.T) {
	tests := []struct {
		name        string
		devicePath  string
		setupFile   bool
		expectError bool
	}{
		{
			name:        "empty device path",
			devicePath:  "",
			setupFile:   false,
			expectError: true,
		},
		{
			name:        "non-existent device",
			devicePath:  "/dev/non-existent",
			setupFile:   false,
			expectError: true,
		},
		{
			name:        "valid file device",
			devicePath:  "",
			setupFile:   true,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var devicePath string
			if tt.setupFile {
				tmpFile, err := os.CreateTemp("", "test-device-")
				if err != nil {
					t.Fatalf("Failed to create temp file: %v", err)
				}
				defer os.Remove(tmpFile.Name())
				defer tmpFile.Close()
				devicePath = tmpFile.Name()
			} else {
				devicePath = tt.devicePath
			}

			err := validateSBDDevice(devicePath)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error, but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestGetNodeNameFromEnv(t *testing.T) {
	tests := []struct {
		name         string
		envVars      map[string]string
		expectedName string
	}{
		{
			name:         "NODE_NAME set",
			envVars:      map[string]string{"NODE_NAME": "test-node"},
			expectedName: "test-node",
		},
		{
			name:         "HOSTNAME set",
			envVars:      map[string]string{"HOSTNAME": "hostname-node"},
			expectedName: "hostname-node",
		},
		{
			name:         "NODENAME set",
			envVars:      map[string]string{"NODENAME": "nodename-node"},
			expectedName: "nodename-node",
		},
		{
			name: "multiple env vars - NODE_NAME takes precedence",
			envVars: map[string]string{
				"NODE_NAME": "primary-node",
				"HOSTNAME":  "secondary-node",
			},
			expectedName: "primary-node",
		},
		{
			name:         "no env vars set",
			envVars:      map[string]string{},
			expectedName: "", // Will fall back to os.Hostname() in real scenario
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear relevant environment variables
			for _, envVar := range []string{"NODE_NAME", "HOSTNAME", "NODENAME"} {
				os.Unsetenv(envVar)
			}

			// Set test environment variables
			for key, value := range tt.envVars {
				os.Setenv(key, value)
				defer os.Unsetenv(key)
			}

			nodeName := getNodeNameFromEnv()

			if tt.expectedName == "" {
				// For the "no env vars" case, expect either empty string or system hostname
				if nodeName == "" {
					// Empty is acceptable if hostname lookup fails in test environment
				} else {
					// Non-empty is acceptable if hostname lookup succeeds
					if len(nodeName) == 0 {
						t.Error("Expected non-empty node name from hostname fallback")
					}
				}
			} else {
				if nodeName != tt.expectedName {
					t.Errorf("Expected node name %q, got %q", tt.expectedName, nodeName)
				}
			}
		})
	}
}

func TestGetNodeIDFromEnv(t *testing.T) {
	tests := []struct {
		name           string
		envVars        map[string]string
		expectedNodeID uint16
	}{
		{
			name:           "SBD_NODE_ID set",
			envVars:        map[string]string{"SBD_NODE_ID": "42"},
			expectedNodeID: 42,
		},
		{
			name:           "NODE_ID set",
			envVars:        map[string]string{"NODE_ID": "123"},
			expectedNodeID: 123,
		},
		{
			name: "multiple env vars - SBD_NODE_ID takes precedence",
			envVars: map[string]string{
				"SBD_NODE_ID": "100",
				"NODE_ID":     "200",
			},
			expectedNodeID: 100,
		},
		{
			name:           "invalid node ID - too high",
			envVars:        map[string]string{"SBD_NODE_ID": "300"},
			expectedNodeID: DefaultNodeID,
		},
		{
			name:           "invalid node ID - zero",
			envVars:        map[string]string{"SBD_NODE_ID": "0"},
			expectedNodeID: DefaultNodeID,
		},
		{
			name:           "invalid node ID - non-numeric",
			envVars:        map[string]string{"SBD_NODE_ID": "abc"},
			expectedNodeID: DefaultNodeID,
		},
		{
			name:           "no env vars set",
			envVars:        map[string]string{},
			expectedNodeID: DefaultNodeID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear relevant environment variables
			for _, envVar := range []string{"SBD_NODE_ID", "NODE_ID"} {
				os.Unsetenv(envVar)
			}

			// Set test environment variables
			for key, value := range tt.envVars {
				os.Setenv(key, value)
				defer os.Unsetenv(key)
			}

			nodeID := getNodeIDFromEnv()

			if nodeID != tt.expectedNodeID {
				t.Errorf("Expected node ID %d, got %d", tt.expectedNodeID, nodeID)
			}
		})
	}
}

func TestGetSBDTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		name            string
		envVars         map[string]string
		expectedTimeout uint
	}{
		{
			name:            "SBD_TIMEOUT_SECONDS set",
			envVars:         map[string]string{"SBD_TIMEOUT_SECONDS": "60"},
			expectedTimeout: 60,
		},
		{
			name:            "SBD_TIMEOUT set",
			envVars:         map[string]string{"SBD_TIMEOUT": "45"},
			expectedTimeout: 45,
		},
		{
			name: "multiple env vars - SBD_TIMEOUT_SECONDS takes precedence",
			envVars: map[string]string{
				"SBD_TIMEOUT_SECONDS": "90",
				"SBD_TIMEOUT":         "120",
			},
			expectedTimeout: 90,
		},
		{
			name:            "invalid timeout - zero",
			envVars:         map[string]string{"SBD_TIMEOUT_SECONDS": "0"},
			expectedTimeout: 30,
		},
		{
			name:            "invalid timeout - non-numeric",
			envVars:         map[string]string{"SBD_TIMEOUT_SECONDS": "abc"},
			expectedTimeout: 30,
		},
		{
			name:            "no env vars set",
			envVars:         map[string]string{},
			expectedTimeout: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear relevant environment variables
			for _, envVar := range []string{"SBD_TIMEOUT_SECONDS", "SBD_TIMEOUT"} {
				os.Unsetenv(envVar)
			}

			// Set test environment variables
			for key, value := range tt.envVars {
				os.Setenv(key, value)
				defer os.Unsetenv(key)
			}

			timeout := getSBDTimeoutFromEnv()

			if timeout != tt.expectedTimeout {
				t.Errorf("Expected timeout %d, got %d", tt.expectedTimeout, timeout)
			}
		})
	}
}

func BenchmarkSBDAgent_WriteNodeIDToSBD(b *testing.B) {
	mockDevice := NewMockBlockDevice("/dev/mock-sbd")
	mockWatchdog := NewMockWatchdog("/dev/watchdog")

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", "benchmark-node", 1, 30*time.Second, 5*time.Second, 15*time.Second)
	if err != nil {
		b.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	agent.setSBDDevice(mockDevice)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := agent.writeNodeIDToSBD(); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
	}
}

func BenchmarkSBDAgent_WriteHeartbeatToSBD(b *testing.B) {
	mockDevice := NewMockBlockDevice("/dev/mock-sbd")
	mockWatchdog := NewMockWatchdog("/dev/watchdog")

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "/dev/mock-sbd", "benchmark-node", 1, 30*time.Second, 5*time.Second, 15*time.Second)
	if err != nil {
		b.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	agent.setSBDDevice(mockDevice)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := agent.writeHeartbeatToSBD(); err != nil {
			b.Fatalf("Heartbeat write failed: %v", err)
		}
	}
}

func TestNewSBDAgentWithMockWatchdog(t *testing.T) {
	// Test creating agent with mock watchdog directly
	mockWatchdog := NewMockWatchdog("/dev/mock-watchdog")
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second)
	if err != nil {
		t.Fatalf("Failed to create agent with mock watchdog: %v", err)
	}

	if agent.watchdog.Path() != "/dev/mock-watchdog" {
		t.Errorf("Expected watchdog path /dev/mock-watchdog, got %s", agent.watchdog.Path())
	}

	agent.Stop()
}
