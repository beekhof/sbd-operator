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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
)

// MockBlockDevice is a mock implementation of BlockDevice for testing
type MockBlockDevice struct {
	data      []byte
	path      string
	closed    bool
	failRead  bool
	failWrite bool
	failSync  bool
	mutex     sync.RWMutex
}

// NewMockBlockDevice creates a new mock block device with the specified size
func NewMockBlockDevice(path string, size int) *MockBlockDevice {
	return &MockBlockDevice{
		data: make([]byte, size),
		path: path,
	}
}

func (m *MockBlockDevice) ReadAt(p []byte, off int64) (int, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if m.closed {
		return 0, errors.New("device is closed")
	}

	if m.failRead {
		return 0, errors.New("mock read failure")
	}

	if off < 0 || off >= int64(len(m.data)) {
		return 0, errors.New("offset out of range")
	}

	n := copy(p, m.data[off:])
	return n, nil
}

func (m *MockBlockDevice) WriteAt(p []byte, off int64) (int, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.closed {
		return 0, errors.New("device is closed")
	}

	if m.failWrite {
		return 0, errors.New("mock write failure")
	}

	if off < 0 || off >= int64(len(m.data)) {
		return 0, errors.New("offset out of range")
	}

	// Ensure we don't write beyond the buffer
	end := off + int64(len(p))
	if end > int64(len(m.data)) {
		end = int64(len(m.data))
	}

	n := copy(m.data[off:end], p)
	return n, nil
}

func (m *MockBlockDevice) Sync() error {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if m.closed {
		return errors.New("device is closed")
	}

	if m.failSync {
		return errors.New("mock sync failure")
	}

	return nil
}

func (m *MockBlockDevice) Close() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.closed = true
	return nil
}

func (m *MockBlockDevice) Path() string {
	return m.path
}

func (m *MockBlockDevice) IsClosed() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.closed
}

// SetFailRead configures the mock to fail read operations
func (m *MockBlockDevice) SetFailRead(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.failRead = fail
}

// SetFailWrite configures the mock to fail write operations
func (m *MockBlockDevice) SetFailWrite(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.failWrite = fail
}

// SetFailSync configures the mock to fail sync operations
func (m *MockBlockDevice) SetFailSync(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.failSync = fail
}

// GetData returns a copy of the current data buffer
func (m *MockBlockDevice) GetData() []byte {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	result := make([]byte, len(m.data))
	copy(result, m.data)
	return result
}

// WritePeerHeartbeat writes a heartbeat message to a specific peer slot for testing
func (m *MockBlockDevice) WritePeerHeartbeat(nodeID uint16, timestamp uint64, sequence uint64) error {
	// Create heartbeat message
	header := sbdprotocol.NewHeartbeat(nodeID, sequence)
	header.Timestamp = timestamp
	heartbeatMsg := sbdprotocol.SBDHeartbeatMessage{Header: header}

	// Marshal the message
	msgBytes, err := sbdprotocol.MarshalHeartbeat(heartbeatMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat message: %w", err)
	}

	// Calculate slot offset for this node
	slotOffset := int64(nodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Write heartbeat message to the designated slot
	_, err = m.WriteAt(msgBytes, slotOffset)
	return err
}

// MockWatchdog is a mock implementation of WatchdogInterface for testing
type MockWatchdog struct {
	path      string
	petCount  int
	closed    bool
	failPet   bool
	failClose bool
	mutex     sync.RWMutex
}

// NewMockWatchdog creates a new mock watchdog
func NewMockWatchdog(path string) *MockWatchdog {
	return &MockWatchdog{
		path: path,
	}
}

func (m *MockWatchdog) Pet() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.closed {
		return errors.New("watchdog is closed")
	}

	if m.failPet {
		return errors.New("mock pet failure")
	}

	m.petCount++
	return nil
}

func (m *MockWatchdog) Close() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.failClose {
		return errors.New("mock close failure")
	}

	m.closed = true
	return nil
}

func (m *MockWatchdog) Path() string {
	return m.path
}

// GetPetCount returns the number of times Pet() was called
func (m *MockWatchdog) GetPetCount() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.petCount
}

// SetFailPet configures the mock to fail pet operations
func (m *MockWatchdog) SetFailPet(fail bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.failPet = fail
}

// TestPeerMonitor tests the peer monitoring functionality
func TestPeerMonitor(t *testing.T) {
	monitor := NewPeerMonitor(30, 1)

	// Initially no peers
	if count := monitor.GetHealthyPeerCount(); count != 0 {
		t.Errorf("Expected 0 healthy peers initially, got %d", count)
	}

	// Update a peer
	monitor.UpdatePeer(2, 1000, 1)

	// Should have one healthy peer
	if count := monitor.GetHealthyPeerCount(); count != 1 {
		t.Errorf("Expected 1 healthy peer, got %d", count)
	}

	// Get peer status
	peers := monitor.GetPeerStatus()
	if len(peers) != 1 {
		t.Errorf("Expected 1 peer in status map, got %d", len(peers))
	}

	peer, exists := peers[2]
	if !exists {
		t.Error("Expected peer 2 to exist in status map")
	}

	if peer.NodeID != 2 {
		t.Errorf("Expected peer NodeID 2, got %d", peer.NodeID)
	}

	if peer.LastTimestamp != 1000 {
		t.Errorf("Expected peer timestamp 1000, got %d", peer.LastTimestamp)
	}

	if peer.LastSequence != 1 {
		t.Errorf("Expected peer sequence 1, got %d", peer.LastSequence)
	}

	if !peer.IsHealthy {
		t.Error("Expected peer to be healthy")
	}
}

func TestPeerMonitor_Liveness(t *testing.T) {
	monitor := NewPeerMonitor(1, 1) // 1 second timeout

	// Update a peer
	monitor.UpdatePeer(2, 1000, 1)

	// Should be healthy initially
	if count := monitor.GetHealthyPeerCount(); count != 1 {
		t.Errorf("Expected 1 healthy peer initially, got %d", count)
	}

	// Wait for timeout
	time.Sleep(1100 * time.Millisecond)

	// Check liveness
	monitor.CheckPeerLiveness()

	// Should now be unhealthy
	if count := monitor.GetHealthyPeerCount(); count != 0 {
		t.Errorf("Expected 0 healthy peers after timeout, got %d", count)
	}

	// Update peer again
	monitor.UpdatePeer(2, 2000, 2)

	// Should be healthy again
	if count := monitor.GetHealthyPeerCount(); count != 1 {
		t.Errorf("Expected 1 healthy peer after update, got %d", count)
	}
}

func TestPeerMonitor_SequenceValidation(t *testing.T) {
	monitor := NewPeerMonitor(30, 1)

	// Update a peer with sequence 5
	monitor.UpdatePeer(2, 1000, 5)

	peers := monitor.GetPeerStatus()
	peer := peers[2]
	if peer.LastSequence != 5 {
		t.Errorf("Expected sequence 5, got %d", peer.LastSequence)
	}

	// Update with older sequence (should be ignored)
	monitor.UpdatePeer(2, 1000, 3)

	peers = monitor.GetPeerStatus()
	peer = peers[2]
	if peer.LastSequence != 5 {
		t.Errorf("Expected sequence to remain 5, got %d", peer.LastSequence)
	}

	// Update with newer sequence (should be accepted)
	monitor.UpdatePeer(2, 1000, 7)

	peers = monitor.GetPeerStatus()
	peer = peers[2]
	if peer.LastSequence != 7 {
		t.Errorf("Expected sequence 7, got %d", peer.LastSequence)
	}
}

func TestSBDAgent_ReadPeerHeartbeat(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Write a peer heartbeat to slot 2
	err = mockDevice.WritePeerHeartbeat(2, 12345, 1)
	if err != nil {
		t.Fatalf("Failed to write peer heartbeat: %v", err)
	}

	// Read the peer heartbeat
	err = agent.readPeerHeartbeat(2)
	if err != nil {
		t.Errorf("Failed to read peer heartbeat: %v", err)
	}

	// Check that peer was added to monitor
	peers := agent.peerMonitor.GetPeerStatus()
	if len(peers) != 1 {
		t.Errorf("Expected 1 peer, got %d", len(peers))
	}

	peer, exists := peers[2]
	if !exists {
		t.Error("Expected peer 2 to exist")
	}

	if peer.LastTimestamp != 12345 {
		t.Errorf("Expected timestamp 12345, got %d", peer.LastTimestamp)
	}

	if peer.LastSequence != 1 {
		t.Errorf("Expected sequence 1, got %d", peer.LastSequence)
	}
}

func TestSBDAgent_ReadPeerHeartbeat_InvalidMessage(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Write invalid data to slot 2
	slotOffset := int64(2) * sbdprotocol.SBD_SLOT_SIZE
	invalidData := []byte("invalid heartbeat data")
	_, err = mockDevice.WriteAt(invalidData, slotOffset)
	if err != nil {
		t.Fatalf("Failed to write invalid data: %v", err)
	}

	// Try to read the peer heartbeat (should not fail, but should not add peer)
	err = agent.readPeerHeartbeat(2)
	if err != nil {
		t.Errorf("Reading invalid heartbeat should not fail: %v", err)
	}

	// Check that no peer was added
	peers := agent.peerMonitor.GetPeerStatus()
	if len(peers) != 0 {
		t.Errorf("Expected 0 peers with invalid data, got %d", len(peers))
	}
}

func TestSBDAgent_ReadPeerHeartbeat_DeviceError(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Configure device to fail reads
	mockDevice.SetFailRead(true)

	// Try to read peer heartbeat (should fail)
	err = agent.readPeerHeartbeat(2)
	if err == nil {
		t.Error("Expected read to fail with device error")
	}

	// Check that no peer was added
	peers := agent.peerMonitor.GetPeerStatus()
	if len(peers) != 0 {
		t.Errorf("Expected 0 peers with device error, got %d", len(peers))
	}
}

func TestSBDAgent_ReadPeerHeartbeat_NodeIDMismatch(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Write a heartbeat with NodeID 3 to slot 2 (mismatch)
	err = mockDevice.WritePeerHeartbeat(3, 12345, 1)
	if err != nil {
		t.Fatalf("Failed to write peer heartbeat: %v", err)
	}

	// Read from slot 2 (should detect mismatch and not add peer)
	err = agent.readPeerHeartbeat(2)
	if err != nil {
		t.Errorf("Reading mismatched heartbeat should not fail: %v", err)
	}

	// Check that no peer was added due to mismatch
	peers := agent.peerMonitor.GetPeerStatus()
	if len(peers) != 0 {
		t.Errorf("Expected 0 peers with NodeID mismatch, got %d", len(peers))
	}
}

func TestSBDAgent_PeerMonitorLoop_Integration(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	// Create agent with short check interval for testing
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 100*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Write heartbeats for multiple peers
	err = mockDevice.WritePeerHeartbeat(2, 12345, 1)
	if err != nil {
		t.Fatalf("Failed to write peer 2 heartbeat: %v", err)
	}

	err = mockDevice.WritePeerHeartbeat(3, 12346, 1)
	if err != nil {
		t.Fatalf("Failed to write peer 3 heartbeat: %v", err)
	}

	// Start the peer monitor loop
	go agent.peerMonitorLoop()

	// Wait for a few check cycles
	time.Sleep(300 * time.Millisecond)

	// Check that peers were discovered
	peers := agent.peerMonitor.GetPeerStatus()
	if len(peers) != 2 {
		t.Errorf("Expected 2 peers, got %d", len(peers))
	}

	if _, exists := peers[2]; !exists {
		t.Error("Expected peer 2 to be discovered")
	}

	if _, exists := peers[3]; !exists {
		t.Error("Expected peer 3 to be discovered")
	}

	// Check healthy peer count
	if count := agent.peerMonitor.GetHealthyPeerCount(); count != 2 {
		t.Errorf("Expected 2 healthy peers, got %d", count)
	}

	// Wait for peers to become unhealthy (1 second timeout + check interval)
	time.Sleep(1200 * time.Millisecond)

	// Should now have 0 healthy peers
	if count := agent.peerMonitor.GetHealthyPeerCount(); count != 0 {
		t.Errorf("Expected 0 healthy peers after timeout, got %d", count)
	}
}

func TestSBDAgent_NewSBDAgent(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "sbd-agent-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	watchdogPath := filepath.Join(tempDir, "watchdog")
	sbdPath := filepath.Join(tempDir, "sbd")

	// Create mock device files
	if err := os.WriteFile(watchdogPath, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create mock watchdog: %v", err)
	}
	if err := os.WriteFile(sbdPath, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create mock SBD device: %v", err)
	}

	agent, err := NewSBDAgent(watchdogPath, sbdPath, "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	if agent.nodeName != "test-node" {
		t.Errorf("Expected node name 'test-node', got '%s'", agent.nodeName)
	}

	if agent.nodeID != 1 {
		t.Errorf("Expected node ID 1, got %d", agent.nodeID)
	}

	if agent.petInterval != 30*time.Second {
		t.Errorf("Expected pet interval 30s, got %s", agent.petInterval)
	}

	if agent.peerCheckInterval != 5*time.Second {
		t.Errorf("Expected peer check interval 5s, got %s", agent.peerCheckInterval)
	}
}

func TestSBDAgent_WriteHeartbeatToSBD(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 2, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Write a heartbeat
	err = agent.writeHeartbeatToSBD()
	if err != nil {
		t.Errorf("Failed to write heartbeat: %v", err)
	}

	// Verify the heartbeat was written to the correct slot
	slotOffset := int64(2) * sbdprotocol.SBD_SLOT_SIZE
	slotData := make([]byte, sbdprotocol.SBD_HEADER_SIZE)
	n, err := mockDevice.ReadAt(slotData, slotOffset)
	if err != nil {
		t.Fatalf("Failed to read written heartbeat: %v", err)
	}
	if n != sbdprotocol.SBD_HEADER_SIZE {
		t.Errorf("Expected to read %d bytes, got %d", sbdprotocol.SBD_HEADER_SIZE, n)
	}

	// Unmarshal and verify the message
	header, err := sbdprotocol.Unmarshal(slotData)
	if err != nil {
		t.Fatalf("Failed to unmarshal written heartbeat: %v", err)
	}

	if header.NodeID != 2 {
		t.Errorf("Expected NodeID 2, got %d", header.NodeID)
	}

	if header.Type != sbdprotocol.SBD_MSG_TYPE_HEARTBEAT {
		t.Errorf("Expected heartbeat type, got %d", header.Type)
	}

	if header.Sequence != 1 {
		t.Errorf("Expected sequence 1, got %d", header.Sequence)
	}
}

func TestSBDAgent_WriteHeartbeatToSBD_DeviceError(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Configure device to fail writes
	mockDevice.SetFailWrite(true)

	// Try to write heartbeat (should fail)
	err = agent.writeHeartbeatToSBD()
	if err == nil {
		t.Error("Expected heartbeat write to fail with device error")
	}
}

func TestSBDAgent_WriteHeartbeatToSBD_SyncError(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Set the mock device
	agent.setSBDDevice(mockDevice)

	// Configure device to fail sync
	mockDevice.SetFailSync(true)

	// Try to write heartbeat (should fail at sync)
	err = agent.writeHeartbeatToSBD()
	if err == nil {
		t.Error("Expected heartbeat write to fail with sync error")
	}

	if !strings.Contains(err.Error(), "sync") {
		t.Errorf("Expected sync error message, got: %v", err)
	}
}

func TestSBDAgent_SBDHealthStatus(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	// Use empty SBD device path so initialization doesn't fail
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// Initial state should be healthy when no SBD device is configured
	if !agent.isSBDHealthy() {
		t.Error("Expected SBD to be initially healthy with no device")
	}

	// Mark as unhealthy
	agent.setSBDHealthy(false)
	if agent.isSBDHealthy() {
		t.Error("Expected SBD to be unhealthy after setting to false")
	}

	// Mark as healthy again
	agent.setSBDHealthy(true)
	if !agent.isSBDHealthy() {
		t.Error("Expected SBD to be healthy after setting to true")
	}
}

func TestSBDAgent_HeartbeatSequence(t *testing.T) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	// First sequence number should be 1
	seq1 := agent.getNextHeartbeatSequence()
	if seq1 != 1 {
		t.Errorf("Expected first sequence to be 1, got %d", seq1)
	}

	// Second sequence number should be 2
	seq2 := agent.getNextHeartbeatSequence()
	if seq2 != 2 {
		t.Errorf("Expected second sequence to be 2, got %d", seq2)
	}

	// Sequences should be incrementing
	if seq2 <= seq1 {
		t.Errorf("Expected sequence to increment: %d -> %d", seq1, seq2)
	}
}

func TestEnvironmentVariables(t *testing.T) {
	// Test getNodeNameFromEnv
	t.Run("getNodeNameFromEnv", func(t *testing.T) {
		// Clear environment
		os.Unsetenv("NODE_NAME")
		os.Unsetenv("HOSTNAME")
		os.Unsetenv("NODENAME")

		// Set NODE_NAME
		os.Setenv("NODE_NAME", "test-env-node")
		defer os.Unsetenv("NODE_NAME")

		nodeName := getNodeNameFromEnv()
		if nodeName != "test-env-node" {
			t.Errorf("Expected 'test-env-node', got '%s'", nodeName)
		}
	})

	// Test getNodeIDFromEnv
	t.Run("getNodeIDFromEnv", func(t *testing.T) {
		// Clear environment
		os.Unsetenv("SBD_NODE_ID")
		os.Unsetenv("NODE_ID")

		// Set SBD_NODE_ID
		os.Setenv("SBD_NODE_ID", "5")
		defer os.Unsetenv("SBD_NODE_ID")

		nodeID := getNodeIDFromEnv()
		if nodeID != 5 {
			t.Errorf("Expected 5, got %d", nodeID)
		}
	})

	// Test getSBDTimeoutFromEnv
	t.Run("getSBDTimeoutFromEnv", func(t *testing.T) {
		// Clear environment
		os.Unsetenv("SBD_TIMEOUT_SECONDS")
		os.Unsetenv("SBD_TIMEOUT")

		// Set SBD_TIMEOUT_SECONDS
		os.Setenv("SBD_TIMEOUT_SECONDS", "60")
		defer os.Unsetenv("SBD_TIMEOUT_SECONDS")

		timeout := getSBDTimeoutFromEnv()
		if timeout != 60 {
			t.Errorf("Expected 60, got %d", timeout)
		}
	})
}

// Benchmark tests
func BenchmarkSBDAgent_WriteHeartbeat(b *testing.B) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		b.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	agent.setSBDDevice(mockDevice)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := agent.writeHeartbeatToSBD(); err != nil {
			b.Fatalf("Failed to write heartbeat: %v", err)
		}
	}
}

func BenchmarkSBDAgent_ReadPeerHeartbeat(b *testing.B) {
	mockWatchdog := NewMockWatchdog("/dev/watchdog")
	mockDevice := NewMockBlockDevice("/dev/mock-sbd", int(sbdprotocol.SBD_MAX_NODES*sbdprotocol.SBD_SLOT_SIZE))

	agent, err := NewSBDAgentWithWatchdog(mockWatchdog, "", "test-node", 1, 30*time.Second, 5*time.Second, 15*time.Second, 5*time.Second, 30)
	if err != nil {
		b.Fatalf("Failed to create agent: %v", err)
	}
	defer agent.Stop()

	agent.setSBDDevice(mockDevice)

	// Write a peer heartbeat
	err = mockDevice.WritePeerHeartbeat(2, 12345, 1)
	if err != nil {
		b.Fatalf("Failed to write peer heartbeat: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := agent.readPeerHeartbeat(2); err != nil {
			b.Fatalf("Failed to read peer heartbeat: %v", err)
		}
	}
}

func BenchmarkPeerMonitor_UpdatePeer(b *testing.B) {
	monitor := NewPeerMonitor(30, 1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		monitor.UpdatePeer(2, uint64(i), uint64(i))
	}
}
