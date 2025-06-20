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
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/medik8s/sbd-operator/pkg/blockdevice"
	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
	"github.com/medik8s/sbd-operator/pkg/watchdog"
)

var (
	watchdogPath      = flag.String("watchdog-path", "/dev/watchdog", "Path to the watchdog device")
	watchdogTimeout   = flag.Duration("watchdog-timeout", 30*time.Second, "Watchdog pet interval")
	sbdDevice         = flag.String("sbd-device", "", "Path to the SBD block device")
	nodeName          = flag.String("node-name", "", "Name of this Kubernetes node")
	nodeID            = flag.Uint("node-id", 0, "Unique numeric ID for this node (1-255)")
	sbdTimeoutSeconds = flag.Uint("sbd-timeout-seconds", 30, "SBD timeout in seconds (determines heartbeat interval)")
	sbdUpdateInterval = flag.Duration("sbd-update-interval", 5*time.Second, "Interval for updating SBD device with node status")
	peerCheckInterval = flag.Duration("peer-check-interval", 5*time.Second, "Interval for checking peer heartbeats")
	logLevel          = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

const (
	// SBDNodeIDOffset is the offset where node ID is written in the SBD device
	SBDNodeIDOffset = 0
	// MaxNodeNameLength is the maximum length for a node name in SBD device
	MaxNodeNameLength = 256
	// DefaultNodeID is the placeholder node ID used when none is specified
	DefaultNodeID = 1
)

// BlockDevice defines the interface for block device operations
type BlockDevice interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Close() error
	Path() string
	IsClosed() bool
}

// WatchdogInterface defines the interface for watchdog operations
type WatchdogInterface interface {
	Pet() error
	Close() error
	Path() string
}

// PeerStatus represents the status of a peer node
type PeerStatus struct {
	NodeID        uint16    `json:"nodeId"`
	LastTimestamp uint64    `json:"lastTimestamp"`
	LastSequence  uint64    `json:"lastSequence"`
	LastSeen      time.Time `json:"lastSeen"`
	IsHealthy     bool      `json:"isHealthy"`
}

// PeerMonitor manages tracking of peer node states
type PeerMonitor struct {
	peers             map[uint16]*PeerStatus
	peersMutex        sync.RWMutex
	sbdTimeoutSeconds uint
	ownNodeID         uint16
}

// NewPeerMonitor creates a new peer monitor instance
func NewPeerMonitor(sbdTimeoutSeconds uint, ownNodeID uint16) *PeerMonitor {
	return &PeerMonitor{
		peers:             make(map[uint16]*PeerStatus),
		sbdTimeoutSeconds: sbdTimeoutSeconds,
		ownNodeID:         ownNodeID,
	}
}

// UpdatePeer updates the status of a peer node
func (pm *PeerMonitor) UpdatePeer(nodeID uint16, timestamp, sequence uint64) {
	pm.peersMutex.Lock()
	defer pm.peersMutex.Unlock()

	now := time.Now()

	// Get or create peer status
	peer, exists := pm.peers[nodeID]
	if !exists {
		peer = &PeerStatus{
			NodeID:    nodeID,
			IsHealthy: true,
		}
		pm.peers[nodeID] = peer
		log.Printf("INFO: Discovered new peer node %d", nodeID)
	}

	// Check if this is a newer heartbeat
	isNewer := false
	if timestamp > peer.LastTimestamp {
		isNewer = true
	} else if timestamp == peer.LastTimestamp && sequence > peer.LastSequence {
		isNewer = true
	}

	if isNewer {
		// Update peer status
		wasHealthy := peer.IsHealthy
		peer.LastTimestamp = timestamp
		peer.LastSequence = sequence
		peer.LastSeen = now
		peer.IsHealthy = true

		// Log status change
		if !wasHealthy {
			log.Printf("INFO: Peer node %d is now healthy (timestamp=%d, sequence=%d)", nodeID, timestamp, sequence)
		} else {
			log.Printf("DEBUG: Updated peer node %d heartbeat (timestamp=%d, sequence=%d)", nodeID, timestamp, sequence)
		}
	}
}

// CheckPeerLiveness checks which peers are still alive based on timeout
func (pm *PeerMonitor) CheckPeerLiveness() {
	pm.peersMutex.Lock()
	defer pm.peersMutex.Unlock()

	now := time.Now()
	timeout := time.Duration(pm.sbdTimeoutSeconds) * time.Second

	for nodeID, peer := range pm.peers {
		timeSinceLastSeen := now.Sub(peer.LastSeen)
		wasHealthy := peer.IsHealthy

		// Consider peer unhealthy if we haven't seen a heartbeat within timeout
		peer.IsHealthy = timeSinceLastSeen <= timeout

		// Log status change
		if wasHealthy && !peer.IsHealthy {
			log.Printf("WARNING: Peer node %d is now unhealthy (last seen %v ago, timeout %v)",
				nodeID, timeSinceLastSeen, timeout)
		} else if !wasHealthy && peer.IsHealthy {
			log.Printf("INFO: Peer node %d is now healthy (last seen %v ago)",
				nodeID, timeSinceLastSeen)
		}
	}
}

// GetPeerStatus returns a copy of the current peer status map
func (pm *PeerMonitor) GetPeerStatus() map[uint16]*PeerStatus {
	pm.peersMutex.RLock()
	defer pm.peersMutex.RUnlock()

	// Return a deep copy to avoid race conditions
	result := make(map[uint16]*PeerStatus)
	for nodeID, peer := range pm.peers {
		result[nodeID] = &PeerStatus{
			NodeID:        peer.NodeID,
			LastTimestamp: peer.LastTimestamp,
			LastSequence:  peer.LastSequence,
			LastSeen:      peer.LastSeen,
			IsHealthy:     peer.IsHealthy,
		}
	}
	return result
}

// GetHealthyPeerCount returns the number of healthy peers
func (pm *PeerMonitor) GetHealthyPeerCount() int {
	pm.peersMutex.RLock()
	defer pm.peersMutex.RUnlock()

	count := 0
	for _, peer := range pm.peers {
		if peer.IsHealthy {
			count++
		}
	}
	return count
}

// SBDAgent represents the main SBD agent application
type SBDAgent struct {
	watchdog          WatchdogInterface
	sbdDevice         BlockDevice
	sbdDevicePath     string
	nodeName          string
	nodeID            uint16
	petInterval       time.Duration
	sbdUpdateInterval time.Duration
	heartbeatInterval time.Duration
	peerCheckInterval time.Duration
	ctx               context.Context
	cancel            context.CancelFunc
	sbdHealthy        bool
	sbdHealthyMutex   sync.RWMutex
	heartbeatSequence uint64
	heartbeatSeqMutex sync.Mutex
	peerMonitor       *PeerMonitor
}

// NewSBDAgent creates a new SBD agent instance
func NewSBDAgent(watchdogPath, sbdDevicePath, nodeName string, nodeID uint16, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval time.Duration, sbdTimeoutSeconds uint) (*SBDAgent, error) {
	// Initialize watchdog
	wd, err := watchdog.New(watchdogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize watchdog: %w", err)
	}

	return NewSBDAgentWithWatchdog(wd, sbdDevicePath, nodeName, nodeID, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval, sbdTimeoutSeconds)
}

// NewSBDAgentWithWatchdog creates a new SBD agent instance with a custom watchdog (for testing)
func NewSBDAgentWithWatchdog(wd WatchdogInterface, sbdDevicePath, nodeName string, nodeID uint16, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval time.Duration, sbdTimeoutSeconds uint) (*SBDAgent, error) {
	ctx, cancel := context.WithCancel(context.Background())

	agent := &SBDAgent{
		watchdog:          wd,
		sbdDevicePath:     sbdDevicePath,
		nodeName:          nodeName,
		nodeID:            nodeID,
		petInterval:       petInterval,
		sbdUpdateInterval: sbdUpdateInterval,
		heartbeatInterval: heartbeatInterval,
		peerCheckInterval: peerCheckInterval,
		ctx:               ctx,
		cancel:            cancel,
		sbdHealthy:        true, // Start assuming healthy
		heartbeatSequence: 0,
		peerMonitor:       NewPeerMonitor(sbdTimeoutSeconds, nodeID),
	}

	// Initialize SBD device if specified
	if sbdDevicePath != "" {
		if err := agent.initializeSBDDevice(); err != nil {
			log.Printf("WARNING: Failed to initialize SBD device: %v", err)
			agent.setSBDHealthy(false)
		}
	}

	return agent, nil
}

// initializeSBDDevice opens and initializes the SBD block device
func (s *SBDAgent) initializeSBDDevice() error {
	if s.sbdDevicePath == "" {
		return fmt.Errorf("SBD device path not specified")
	}

	device, err := blockdevice.Open(s.sbdDevicePath)
	if err != nil {
		return fmt.Errorf("failed to open SBD device %s: %w", s.sbdDevicePath, err)
	}

	s.sbdDevice = device
	log.Printf("Successfully opened SBD device: %s", s.sbdDevicePath)
	return nil
}

// setSBDDevice allows setting a custom SBD device (useful for testing)
func (s *SBDAgent) setSBDDevice(device BlockDevice) {
	s.sbdDevice = device
}

// setSBDHealthy safely updates the SBD health status
func (s *SBDAgent) setSBDHealthy(healthy bool) {
	s.sbdHealthyMutex.Lock()
	defer s.sbdHealthyMutex.Unlock()
	s.sbdHealthy = healthy
}

// isSBDHealthy safely reads the SBD health status
func (s *SBDAgent) isSBDHealthy() bool {
	s.sbdHealthyMutex.RLock()
	defer s.sbdHealthyMutex.RUnlock()
	return s.sbdHealthy
}

// getNextHeartbeatSequence safely increments and returns the next sequence number
func (s *SBDAgent) getNextHeartbeatSequence() uint64 {
	s.heartbeatSeqMutex.Lock()
	defer s.heartbeatSeqMutex.Unlock()
	s.heartbeatSequence++
	return s.heartbeatSequence
}

// writeNodeIDToSBD writes the node name to the SBD device at the predefined offset
func (s *SBDAgent) writeNodeIDToSBD() error {
	if s.sbdDevice == nil || s.sbdDevice.IsClosed() {
		// Try to reinitialize the device
		if err := s.initializeSBDDevice(); err != nil {
			return fmt.Errorf("SBD device is closed and reinitialize failed: %w", err)
		}
	}

	// Prepare node name data with fixed size
	nodeData := make([]byte, MaxNodeNameLength)
	copy(nodeData, []byte(s.nodeName))

	// Write node name to SBD device
	n, err := s.sbdDevice.WriteAt(nodeData, SBDNodeIDOffset)
	if err != nil {
		return fmt.Errorf("failed to write node ID to SBD device: %w", err)
	}

	if n != len(nodeData) {
		return fmt.Errorf("partial write to SBD device: wrote %d bytes, expected %d", n, len(nodeData))
	}

	// Ensure data is committed to storage
	if err := s.sbdDevice.Sync(); err != nil {
		return fmt.Errorf("failed to sync SBD device: %w", err)
	}

	return nil
}

// writeHeartbeatToSBD writes a heartbeat message to the node's designated slot
func (s *SBDAgent) writeHeartbeatToSBD() error {
	if s.sbdDevice == nil || s.sbdDevice.IsClosed() {
		// Try to reinitialize the device
		if err := s.initializeSBDDevice(); err != nil {
			return fmt.Errorf("SBD device is closed and reinitialize failed: %w", err)
		}
	}

	// Create heartbeat message
	sequence := s.getNextHeartbeatSequence()
	heartbeatHeader := sbdprotocol.NewHeartbeat(s.nodeID, sequence)
	heartbeatMsg := sbdprotocol.SBDHeartbeatMessage{Header: heartbeatHeader}

	// Marshal the message
	msgBytes, err := sbdprotocol.MarshalHeartbeat(heartbeatMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat message: %w", err)
	}

	// Calculate slot offset for this node (NodeID * SBD_SLOT_SIZE)
	slotOffset := int64(s.nodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Write heartbeat message to the designated slot
	n, err := s.sbdDevice.WriteAt(msgBytes, slotOffset)
	if err != nil {
		return fmt.Errorf("failed to write heartbeat to SBD device at offset %d: %w", slotOffset, err)
	}

	if n != len(msgBytes) {
		return fmt.Errorf("partial write to SBD device: wrote %d bytes, expected %d", n, len(msgBytes))
	}

	// Ensure data is committed to storage
	if err := s.sbdDevice.Sync(); err != nil {
		return fmt.Errorf("failed to sync SBD device after heartbeat write: %w", err)
	}

	log.Printf("DEBUG: Successfully wrote heartbeat message (seq=%d) to slot %d", sequence, s.nodeID)
	return nil
}

// readPeerHeartbeat reads and processes a heartbeat from a peer node's slot
func (s *SBDAgent) readPeerHeartbeat(peerNodeID uint16) error {
	if s.sbdDevice == nil || s.sbdDevice.IsClosed() {
		return fmt.Errorf("SBD device is not available")
	}

	// Calculate slot offset for the peer node
	slotOffset := int64(peerNodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Read the entire slot
	slotData := make([]byte, sbdprotocol.SBD_SLOT_SIZE)
	n, err := s.sbdDevice.ReadAt(slotData, slotOffset)
	if err != nil {
		return fmt.Errorf("failed to read peer %d heartbeat from offset %d: %w", peerNodeID, slotOffset, err)
	}

	if n != sbdprotocol.SBD_SLOT_SIZE {
		return fmt.Errorf("partial read from peer %d slot: read %d bytes, expected %d", peerNodeID, n, sbdprotocol.SBD_SLOT_SIZE)
	}

	// Try to unmarshal the message header
	header, err := sbdprotocol.Unmarshal(slotData[:sbdprotocol.SBD_HEADER_SIZE])
	if err != nil {
		// Don't log as error since empty slots are expected
		log.Printf("DEBUG: Failed to unmarshal peer %d heartbeat: %v", peerNodeID, err)
		return nil
	}

	// Validate the message
	if !sbdprotocol.IsValidMessageType(header.Type) {
		log.Printf("DEBUG: Invalid message type from peer %d: %d", peerNodeID, header.Type)
		return nil
	}

	if header.Type != sbdprotocol.SBD_MSG_TYPE_HEARTBEAT {
		log.Printf("DEBUG: Non-heartbeat message from peer %d: type %d", peerNodeID, header.Type)
		return nil
	}

	if header.NodeID != peerNodeID {
		log.Printf("WARNING: NodeID mismatch in peer %d slot: expected %d, got %d", peerNodeID, peerNodeID, header.NodeID)
		return nil
	}

	// Update peer status
	s.peerMonitor.UpdatePeer(peerNodeID, header.Timestamp, header.Sequence)
	return nil
}

// Start begins the SBD agent operations
func (s *SBDAgent) Start() error {
	log.Printf("Starting SBD Agent...")
	log.Printf("Watchdog device: %s", s.watchdog.Path())
	log.Printf("SBD device: %s", s.sbdDevicePath)
	log.Printf("Node name: %s", s.nodeName)
	log.Printf("Node ID: %d", s.nodeID)
	log.Printf("Pet interval: %s", s.petInterval)
	log.Printf("SBD update interval: %s", s.sbdUpdateInterval)
	log.Printf("Heartbeat interval: %s", s.heartbeatInterval)
	log.Printf("Peer check interval: %s", s.peerCheckInterval)

	// Start the watchdog monitoring goroutine
	go s.watchdogLoop()

	// Start SBD device monitoring if available
	if s.sbdDevicePath != "" {
		go s.sbdDeviceLoop()
		go s.heartbeatLoop()
		go s.peerMonitorLoop()
	}

	log.Printf("SBD Agent started successfully")
	return nil
}

// Stop gracefully shuts down the SBD agent
func (s *SBDAgent) Stop() error {
	log.Printf("Stopping SBD Agent...")

	// Cancel context to stop all goroutines
	s.cancel()

	// Close SBD device
	if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
		if err := s.sbdDevice.Close(); err != nil {
			log.Printf("Error closing SBD device: %v", err)
		}
	}

	// Close watchdog device
	if s.watchdog != nil {
		if err := s.watchdog.Close(); err != nil {
			log.Printf("Error closing watchdog: %v", err)
		}
	}

	log.Printf("SBD Agent stopped")
	return nil
}

// watchdogLoop continuously pets the watchdog to prevent system reset
func (s *SBDAgent) watchdogLoop() {
	ticker := time.NewTicker(s.petInterval)
	defer ticker.Stop()

	log.Printf("Starting watchdog loop with interval %s", s.petInterval)

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("Watchdog loop stopping...")
			return
		case <-ticker.C:
			// Only pet the watchdog if SBD device is healthy (or not configured)
			if s.sbdDevicePath == "" || s.isSBDHealthy() {
				if err := s.watchdog.Pet(); err != nil {
					log.Printf("ERROR: Failed to pet watchdog: %v", err)
					// Continue trying - don't exit on pet failure
				} else {
					log.Printf("DEBUG: Watchdog pet successful")
				}
			} else {
				log.Printf("WARNING: Skipping watchdog pet - SBD device is unhealthy")
				// This will cause the system to reboot via watchdog timeout
				// This is the desired behavior for self-fencing when SBD fails
			}
		}
	}
}

// sbdDeviceLoop continuously updates the SBD device with node status
func (s *SBDAgent) sbdDeviceLoop() {
	ticker := time.NewTicker(s.sbdUpdateInterval)
	defer ticker.Stop()

	log.Printf("Starting SBD device loop with interval %s", s.sbdUpdateInterval)

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("SBD device loop stopping...")
			return
		case <-ticker.C:
			if err := s.writeNodeIDToSBD(); err != nil {
				log.Printf("ERROR: Failed to write node ID to SBD device: %v", err)
				s.setSBDHealthy(false)

				// Try to reinitialize the device on next iteration
				if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
					if closeErr := s.sbdDevice.Close(); closeErr != nil {
						log.Printf("ERROR: Failed to close SBD device: %v", closeErr)
					}
				}
			} else {
				log.Printf("DEBUG: Successfully updated SBD device with node ID")
				s.setSBDHealthy(true)
			}
		}
	}
}

// heartbeatLoop continuously writes heartbeat messages to the SBD device
func (s *SBDAgent) heartbeatLoop() {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	log.Printf("Starting SBD heartbeat loop with interval %s", s.heartbeatInterval)

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("SBD heartbeat loop stopping...")
			return
		case <-ticker.C:
			if err := s.writeHeartbeatToSBD(); err != nil {
				log.Printf("ERROR: Failed to write heartbeat to SBD device: %v", err)
				s.setSBDHealthy(false)

				// Try to reinitialize the device on next iteration
				if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
					if closeErr := s.sbdDevice.Close(); closeErr != nil {
						log.Printf("ERROR: Failed to close SBD device during heartbeat error: %v", closeErr)
					}
				}
			} else {
				// Only mark as healthy if it was previously unhealthy
				// The regular SBD device loop will also update this
				if !s.isSBDHealthy() {
					log.Printf("DEBUG: SBD device recovered during heartbeat write")
					s.setSBDHealthy(true)
				}
			}
		}
	}
}

// peerMonitorLoop continuously reads peer heartbeats and checks liveness
func (s *SBDAgent) peerMonitorLoop() {
	ticker := time.NewTicker(s.peerCheckInterval)
	defer ticker.Stop()

	log.Printf("Starting peer monitor loop with interval %s", s.peerCheckInterval)

	for {
		select {
		case <-s.ctx.Done():
			log.Printf("Peer monitor loop stopping...")
			return
		case <-ticker.C:
			// Read heartbeats from all peer slots
			for peerNodeID := uint16(1); peerNodeID <= sbdprotocol.SBD_MAX_NODES; peerNodeID++ {
				// Skip our own slot
				if peerNodeID == s.nodeID {
					continue
				}

				if err := s.readPeerHeartbeat(peerNodeID); err != nil {
					log.Printf("DEBUG: Error reading peer %d heartbeat: %v", peerNodeID, err)
					// Continue with other peers even if one fails
				}
			}

			// Check liveness of all tracked peers
			s.peerMonitor.CheckPeerLiveness()

			// Log cluster status periodically
			healthyPeers := s.peerMonitor.GetHealthyPeerCount()
			log.Printf("DEBUG: Cluster status: %d healthy peers", healthyPeers)
		}
	}
}

// validateSBDDevice checks if the SBD device is accessible
func validateSBDDevice(devicePath string) error {
	if devicePath == "" {
		return fmt.Errorf("SBD device path cannot be empty")
	}

	// Check if device exists
	info, err := os.Stat(devicePath)
	if err != nil {
		return fmt.Errorf("SBD device not accessible: %w", err)
	}

	// Check if it's a block device
	if info.Mode()&os.ModeDevice == 0 {
		log.Printf("WARNING: %s is not a device file", devicePath)
	}

	return nil
}

// getNodeNameFromEnv gets the node name from environment variables if not provided via flag
func getNodeNameFromEnv() string {
	// Try various common environment variables
	envVars := []string{"NODE_NAME", "HOSTNAME", "NODENAME"}

	for _, envVar := range envVars {
		if value := os.Getenv(envVar); value != "" {
			return value
		}
	}

	// Fallback to hostname
	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}

	return ""
}

// getNodeIDFromEnv gets the node ID from environment variables if not provided via flag
func getNodeIDFromEnv() uint16 {
	// Try various environment variable names
	envVars := []string{"SBD_NODE_ID", "NODE_ID"}

	for _, envVar := range envVars {
		if value := os.Getenv(envVar); value != "" {
			if id, err := strconv.ParseUint(value, 10, 16); err == nil && id >= 1 && id <= sbdprotocol.SBD_MAX_NODES {
				return uint16(id)
			}
		}
	}

	return DefaultNodeID
}

// getSBDTimeoutFromEnv gets the SBD timeout from environment variables if not provided via flag
func getSBDTimeoutFromEnv() uint {
	envVars := []string{"SBD_TIMEOUT_SECONDS", "SBD_TIMEOUT"}

	for _, envVar := range envVars {
		if value := os.Getenv(envVar); value != "" {
			if timeout, err := strconv.ParseUint(value, 10, 32); err == nil && timeout > 0 {
				return uint(timeout)
			}
		}
	}

	return 30 // Default timeout
}

func main() {
	flag.Parse()

	log.Printf("SBD Agent starting...")
	log.Printf("Version: development")

	// Determine node name
	nodeNameValue := *nodeName
	if nodeNameValue == "" {
		nodeNameValue = getNodeNameFromEnv()
		if nodeNameValue == "" {
			log.Fatalf("Node name must be specified via --node-name flag or NODE_NAME environment variable")
		}
		log.Printf("Using node name from environment: %s", nodeNameValue)
	}

	// Validate node name length
	if len(nodeNameValue) > MaxNodeNameLength {
		log.Fatalf("Node name too long: %d bytes (max %d)", len(nodeNameValue), MaxNodeNameLength)
	}

	// Determine node ID
	nodeIDValue := uint16(*nodeID)
	if nodeIDValue == 0 {
		nodeIDValue = getNodeIDFromEnv()
		log.Printf("Using node ID from environment or default: %d", nodeIDValue)
	}

	// Validate node ID
	if nodeIDValue < 1 || nodeIDValue > sbdprotocol.SBD_MAX_NODES {
		log.Fatalf("Node ID must be between 1 and %d, got: %d", sbdprotocol.SBD_MAX_NODES, nodeIDValue)
	}

	// Determine SBD timeout
	sbdTimeoutValue := *sbdTimeoutSeconds
	if sbdTimeoutValue == 30 { // Check if it's still the default
		sbdTimeoutValue = getSBDTimeoutFromEnv()
		log.Printf("Using SBD timeout from environment or default: %d seconds", sbdTimeoutValue)
	}

	// Calculate heartbeat interval (sbdTimeoutSeconds / 2)
	heartbeatInterval := time.Duration(sbdTimeoutValue/2) * time.Second
	if heartbeatInterval < time.Second {
		heartbeatInterval = time.Second // Minimum 1 second interval
	}

	// Validate required parameters
	if *sbdDevice == "" {
		log.Printf("WARNING: No SBD device specified, running in watchdog-only mode")
	} else {
		if err := validateSBDDevice(*sbdDevice); err != nil {
			log.Fatalf("SBD device validation failed: %v", err)
		}
	}

	// Create SBD agent
	agent, err := NewSBDAgent(*watchdogPath, *sbdDevice, nodeNameValue, nodeIDValue, *watchdogTimeout, *sbdUpdateInterval, heartbeatInterval, *peerCheckInterval, sbdTimeoutValue)
	if err != nil {
		log.Fatalf("Failed to create SBD agent: %v", err)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the agent
	if err := agent.Start(); err != nil {
		log.Fatalf("Failed to start SBD agent: %v", err)
	}

	// Wait for shutdown signal
	sig := <-sigChan
	log.Printf("Received signal %s, shutting down...", sig)

	// Stop the agent
	if err := agent.Stop(); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Printf("SBD Agent shutdown complete")
}
