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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

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
	rebootMethod      = flag.String("reboot-method", "panic", "Method to use for self-fencing (panic, systemctl-reboot)")
	metricsPort       = flag.Int("metrics-port", 8080, "Port for Prometheus metrics endpoint")
)

const (
	// SBDNodeIDOffset is the offset where node ID is written in the SBD device
	SBDNodeIDOffset = 0
	// MaxNodeNameLength is the maximum length for a node name in SBD device
	MaxNodeNameLength = 256
	// DefaultNodeID is the placeholder node ID used when none is specified
	DefaultNodeID = 1
)

// Global logger instance
var logger logr.Logger

// initializeLogger initializes the structured logger with the specified log level
func initializeLogger(level string) error {
	// Parse log level
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn", "warning":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		return fmt.Errorf("invalid log level: %s (valid: debug, info, warn, error)", level)
	}

	// Create zap config
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapLevel)
	config.Development = false
	config.Encoding = "json"
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.LevelKey = "level"
	config.EncoderConfig.MessageKey = "message"
	config.EncoderConfig.CallerKey = "caller"
	config.EncoderConfig.StacktraceKey = "stacktrace"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.EncodeLevel = zapcore.LowercaseLevelEncoder
	config.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder

	// Build the logger
	zapLogger, err := config.Build()
	if err != nil {
		return fmt.Errorf("failed to build logger: %w", err)
	}

	// Create logr logger from zap
	logger = zapr.NewLogger(zapLogger)

	return nil
}

// Prometheus metrics definitions
var (
	// sbd_agent_status_healthy: 1 if the agent is healthy, 0 otherwise
	// This metric indicates overall agent health including watchdog and SBD device access
	agentHealthyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sbd_agent_status_healthy",
		Help: "SBD Agent health status (1 = healthy, 0 = unhealthy)",
	})

	// sbd_device_io_errors_total: Total number of I/O errors with shared SBD device
	// This metric tracks all I/O operation failures when interacting with the SBD device
	sbdIOErrorsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sbd_device_io_errors_total",
		Help: "Total number of I/O errors encountered when interacting with the shared SBD device",
	})

	// sbd_watchdog_pets_total: Total number of successful watchdog pets
	// This metric counts how many times the kernel watchdog has been successfully petted
	watchdogPetsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sbd_watchdog_pets_total",
		Help: "Total number of times the local kernel watchdog has been successfully petted",
	})

	// sbd_peer_status: Current liveness status of each peer node
	// This metric uses labels to track the status of each peer node in the cluster
	peerStatusGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sbd_peer_status",
		Help: "Current liveness status of each peer node (1 = alive, 0 = unhealthy/down)",
	}, []string{"node_id", "node_name", "status"})

	// sbd_self_fenced_total: Total number of self-fence initiations
	// This metric counts how many times this agent has initiated self-fencing
	selfFencedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sbd_self_fenced_total",
		Help: "Total number of times the agent has initiated a self-fence",
	})
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
	logger            logr.Logger
}

// NewPeerMonitor creates a new peer monitor instance
func NewPeerMonitor(sbdTimeoutSeconds uint, ownNodeID uint16, logger logr.Logger) *PeerMonitor {
	return &PeerMonitor{
		peers:             make(map[uint16]*PeerStatus),
		sbdTimeoutSeconds: sbdTimeoutSeconds,
		ownNodeID:         ownNodeID,
		logger:            logger.WithName("peer-monitor"),
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
		pm.logger.Info("Discovered new peer node",
			"nodeID", nodeID,
			"timestamp", timestamp,
			"sequence", sequence)
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

		// Update Prometheus metrics
		pm.updatePeerMetrics(nodeID, peer.IsHealthy)

		// Log status change
		if !wasHealthy {
			pm.logger.Info("Peer node recovered to healthy status",
				"nodeID", nodeID,
				"timestamp", timestamp,
				"sequence", sequence,
				"lastSeen", peer.LastSeen)
		} else {
			pm.logger.V(1).Info("Updated peer node heartbeat",
				"nodeID", nodeID,
				"timestamp", timestamp,
				"sequence", sequence,
				"lastSeen", peer.LastSeen)
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

		// Update metrics if status changed
		if wasHealthy != peer.IsHealthy {
			pm.updatePeerMetrics(nodeID, peer.IsHealthy)
		}

		// Log status change
		if wasHealthy && !peer.IsHealthy {
			pm.logger.Error(nil, "Peer node became unhealthy",
				"nodeID", nodeID,
				"timeSinceLastSeen", timeSinceLastSeen,
				"timeout", timeout,
				"lastTimestamp", peer.LastTimestamp,
				"lastSequence", peer.LastSequence)
		} else if !wasHealthy && peer.IsHealthy {
			pm.logger.Info("Peer node recovered to healthy status",
				"nodeID", nodeID,
				"timeSinceLastSeen", timeSinceLastSeen,
				"lastTimestamp", peer.LastTimestamp,
				"lastSequence", peer.LastSequence)
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

// updatePeerMetrics updates Prometheus metrics for peer status
func (pm *PeerMonitor) updatePeerMetrics(nodeID uint16, isHealthy bool) {
	nodeIDStr := fmt.Sprintf("%d", nodeID)
	nodeName := fmt.Sprintf("node-%d", nodeID) // Simple node name mapping

	// Set the metric value based on health status
	if isHealthy {
		peerStatusGaugeVec.WithLabelValues(nodeIDStr, nodeName, "alive").Set(1)
		peerStatusGaugeVec.WithLabelValues(nodeIDStr, nodeName, "unhealthy").Set(0)
	} else {
		peerStatusGaugeVec.WithLabelValues(nodeIDStr, nodeName, "alive").Set(0)
		peerStatusGaugeVec.WithLabelValues(nodeIDStr, nodeName, "unhealthy").Set(1)
	}
}

// SBDAgent represents the main SBD agent with self-fencing capabilities
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
	rebootMethod      string
	ctx               context.Context
	cancel            context.CancelFunc
	sbdHealthy        bool
	sbdHealthyMutex   sync.RWMutex
	heartbeatSequence uint64
	heartbeatSeqMutex sync.Mutex
	peerMonitor       *PeerMonitor
	selfFenceDetected bool
	selfFenceMutex    sync.RWMutex
	metricsPort       int
	metricsServer     *http.Server
}

// NewSBDAgent creates a new SBD agent with the specified parameters
func NewSBDAgent(watchdogPath, sbdDevicePath, nodeName string, nodeID uint16, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval time.Duration, sbdTimeoutSeconds uint, rebootMethod string, metricsPort int) (*SBDAgent, error) {
	wd, err := watchdog.New(watchdogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create watchdog: %w", err)
	}

	return NewSBDAgentWithWatchdog(wd, sbdDevicePath, nodeName, nodeID, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval, sbdTimeoutSeconds, rebootMethod, metricsPort)
}

// NewSBDAgentWithWatchdog creates a new SBD agent with the specified watchdog instance
func NewSBDAgentWithWatchdog(wd WatchdogInterface, sbdDevicePath, nodeName string, nodeID uint16, petInterval, sbdUpdateInterval, heartbeatInterval, peerCheckInterval time.Duration, sbdTimeoutSeconds uint, rebootMethod string, metricsPort int) (*SBDAgent, error) {
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
		rebootMethod:      rebootMethod,
		ctx:               ctx,
		cancel:            cancel,
		sbdHealthy:        false,
		heartbeatSequence: 0,
		peerMonitor:       NewPeerMonitor(sbdTimeoutSeconds, nodeID, logger),
		selfFenceDetected: false,
		metricsPort:       metricsPort,
	}

	// Initialize Prometheus metrics
	if err := agent.initMetrics(); err != nil {
		logger.Error(err, "Failed to initialize metrics")
	}

	// Initialize SBD device if provided
	if sbdDevicePath != "" {
		if err := agent.initializeSBDDevice(); err != nil {
			cancel()
			return nil, fmt.Errorf("failed to initialize SBD device: %w", err)
		}
	}

	return agent, nil
}

// initMetrics initializes Prometheus metrics and starts the metrics server
func (s *SBDAgent) initMetrics() error {
	// Register all metrics with the default registry
	prometheus.MustRegister(agentHealthyGauge)
	prometheus.MustRegister(sbdIOErrorsCounter)
	prometheus.MustRegister(watchdogPetsCounter)
	prometheus.MustRegister(peerStatusGaugeVec)
	prometheus.MustRegister(selfFencedCounter)

	// Initialize agent healthy status to 1 (healthy by default)
	agentHealthyGauge.Set(1)

	// Set up the HTTP server for metrics
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	s.metricsServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.metricsPort),
		Handler: mux,
	}

	// Start the metrics server in a goroutine
	go func() {
		logger.Info("Starting Prometheus metrics server", "port", s.metricsPort)
		if err := s.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "Metrics server failed", "port", s.metricsPort)
		}
	}()

	return nil
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
	logger.Info("Successfully opened SBD device", "devicePath", s.sbdDevicePath)
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

	logger.V(1).Info("Successfully wrote heartbeat message",
		"sequence", sequence,
		"nodeID", s.nodeID,
		"slotOffset", slotOffset)
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
		// Increment SBD I/O errors counter for read failures
		sbdIOErrorsCounter.Inc()
		return fmt.Errorf("failed to read peer %d heartbeat from offset %d: %w", peerNodeID, slotOffset, err)
	}

	if n != sbdprotocol.SBD_SLOT_SIZE {
		return fmt.Errorf("partial read from peer %d slot: read %d bytes, expected %d", peerNodeID, n, sbdprotocol.SBD_SLOT_SIZE)
	}

	// Try to unmarshal the message header
	header, err := sbdprotocol.Unmarshal(slotData[:sbdprotocol.SBD_HEADER_SIZE])
	if err != nil {
		// Don't log as error since empty slots are expected
		logger.V(1).Info("Failed to unmarshal peer heartbeat",
			"peerNodeID", peerNodeID,
			"error", err)
		return nil
	}

	// Validate the message
	if !sbdprotocol.IsValidMessageType(header.Type) {
		logger.V(1).Info("Invalid message type from peer",
			"peerNodeID", peerNodeID,
			"messageType", header.Type)
		return nil
	}

	if header.Type != sbdprotocol.SBD_MSG_TYPE_HEARTBEAT {
		logger.V(1).Info("Non-heartbeat message from peer",
			"peerNodeID", peerNodeID,
			"messageType", header.Type)
		return nil
	}

	if header.NodeID != peerNodeID {
		logger.Error(nil, "NodeID mismatch in peer slot",
			"peerNodeID", peerNodeID,
			"expected", peerNodeID,
			"actual", header.NodeID)
		return nil
	}

	// Update peer status
	s.peerMonitor.UpdatePeer(peerNodeID, header.Timestamp, header.Sequence)
	return nil
}

// Start begins the SBD agent operations
func (s *SBDAgent) Start() error {
	logger.Info("Starting SBD Agent",
		"watchdogDevice", s.watchdog.Path(),
		"sbdDevice", s.sbdDevicePath,
		"nodeName", s.nodeName,
		"nodeID", s.nodeID,
		"petInterval", s.petInterval,
		"sbdUpdateInterval", s.sbdUpdateInterval,
		"heartbeatInterval", s.heartbeatInterval,
		"peerCheckInterval", s.peerCheckInterval)

	// Start the watchdog monitoring goroutine
	go s.watchdogLoop()

	// Start SBD device monitoring if available
	if s.sbdDevicePath != "" {
		go s.sbdDeviceLoop()
		go s.heartbeatLoop()
		go s.peerMonitorLoop()
	}

	logger.Info("SBD Agent started successfully")
	return nil
}

// Stop gracefully shuts down the SBD agent
func (s *SBDAgent) Stop() error {
	logger.Info("Stopping SBD Agent")

	// Cancel context to stop all goroutines
	s.cancel()

	// Shutdown metrics server
	if s.metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.metricsServer.Shutdown(ctx); err != nil {
			logger.Error(err, "Error shutting down metrics server")
		}
	}

	// Close SBD device
	if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
		if err := s.sbdDevice.Close(); err != nil {
			logger.Error(err, "Error closing SBD device", "devicePath", s.sbdDevicePath)
		}
	}

	// Close watchdog device
	if s.watchdog != nil {
		if err := s.watchdog.Close(); err != nil {
			logger.Error(err, "Error closing watchdog", "watchdogPath", s.watchdog.Path())
		}
	}

	logger.Info("SBD Agent stopped")
	return nil
}

// watchdogLoop continuously pets the watchdog to prevent system reset
func (s *SBDAgent) watchdogLoop() {
	ticker := time.NewTicker(s.petInterval)
	defer ticker.Stop()

	logger.Info("Starting watchdog loop", "interval", s.petInterval)

	for {
		select {
		case <-s.ctx.Done():
			logger.Info("Watchdog loop stopping")
			return
		case <-ticker.C:
			// Never pet the watchdog if self-fence has been detected
			if s.isSelfFenceDetected() {
				logger.Error(nil, "Self-fence detected - STOPPING watchdog petting to allow system reboot")
				return
			}

			// Only pet the watchdog if SBD device is healthy (or not configured)
			if s.sbdDevicePath == "" || s.isSBDHealthy() {
				if err := s.watchdog.Pet(); err != nil {
					logger.Error(err, "Failed to pet watchdog", "watchdogPath", s.watchdog.Path())
					// Mark agent as unhealthy on watchdog failures
					agentHealthyGauge.Set(0)
					// Continue trying - don't exit on pet failure
				} else {
					logger.V(1).Info("Watchdog pet successful", "watchdogPath", s.watchdog.Path())
					// Increment successful watchdog pets counter
					watchdogPetsCounter.Inc()
					// Update agent health status based on SBD health
					if s.sbdDevicePath == "" || s.isSBDHealthy() {
						agentHealthyGauge.Set(1)
					}
				}
			} else {
				logger.Error(nil, "Skipping watchdog pet - SBD device is unhealthy",
					"sbdDevicePath", s.sbdDevicePath,
					"sbdHealthy", s.isSBDHealthy())
				// Mark agent as unhealthy when SBD device is unhealthy
				agentHealthyGauge.Set(0)
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

	logger.Info("Starting SBD device loop", "interval", s.sbdUpdateInterval)

	for {
		select {
		case <-s.ctx.Done():
			logger.Info("SBD device loop stopping")
			return
		case <-ticker.C:
			if err := s.writeNodeIDToSBD(); err != nil {
				logger.Error(err, "Failed to write node ID to SBD device",
					"devicePath", s.sbdDevicePath,
					"nodeID", s.nodeID)
				s.setSBDHealthy(false)
				// Increment SBD I/O errors counter
				sbdIOErrorsCounter.Inc()
				// Mark agent as unhealthy
				agentHealthyGauge.Set(0)

				// Try to reinitialize the device on next iteration
				if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
					if closeErr := s.sbdDevice.Close(); closeErr != nil {
						logger.Error(closeErr, "Failed to close SBD device", "devicePath", s.sbdDevicePath)
					}
				}
			} else {
				logger.V(1).Info("Successfully updated SBD device with node ID",
					"devicePath", s.sbdDevicePath,
					"nodeID", s.nodeID)
				s.setSBDHealthy(true)
				// Update agent health status
				agentHealthyGauge.Set(1)
			}
		}
	}
}

// heartbeatLoop continuously writes heartbeat messages to the SBD device
func (s *SBDAgent) heartbeatLoop() {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	logger.Info("Starting SBD heartbeat loop", "interval", s.heartbeatInterval)

	for {
		select {
		case <-s.ctx.Done():
			logger.Info("SBD heartbeat loop stopping")
			return
		case <-ticker.C:
			if err := s.writeHeartbeatToSBD(); err != nil {
				logger.Error(err, "Failed to write heartbeat to SBD device",
					"devicePath", s.sbdDevicePath,
					"nodeID", s.nodeID)
				s.setSBDHealthy(false)
				// Increment SBD I/O errors counter
				sbdIOErrorsCounter.Inc()
				// Mark agent as unhealthy
				agentHealthyGauge.Set(0)

				// Try to reinitialize the device on next iteration
				if s.sbdDevice != nil && !s.sbdDevice.IsClosed() {
					if closeErr := s.sbdDevice.Close(); closeErr != nil {
						logger.Error(closeErr, "Failed to close SBD device during heartbeat error",
							"devicePath", s.sbdDevicePath)
					}
				}
			} else {
				// Only mark as healthy if it was previously unhealthy
				// The regular SBD device loop will also update this
				if !s.isSBDHealthy() {
					logger.Info("SBD device recovered during heartbeat write", "devicePath", s.sbdDevicePath)
					s.setSBDHealthy(true)
					// Update agent health status
					agentHealthyGauge.Set(1)
				}
			}
		}
	}
}

// peerMonitorLoop continuously reads peer heartbeats and checks liveness
func (s *SBDAgent) peerMonitorLoop() {
	ticker := time.NewTicker(s.peerCheckInterval)
	defer ticker.Stop()

	logger.Info("Starting peer monitor loop", "interval", s.peerCheckInterval)

	for {
		select {
		case <-s.ctx.Done():
			logger.Info("Peer monitor loop stopping")
			return
		case <-ticker.C:
			// First, check our own slot for fence messages directed at us
			if err := s.readOwnSlotForFenceMessage(); err != nil {
				logger.Info("Error reading own slot for fence messages", "error", err)
			}

			// If self-fence was detected, stop all operations
			if s.isSelfFenceDetected() {
				logger.Error(nil, "Self-fence detected in peer monitor loop - stopping all operations")
				return
			}

			// Read heartbeats from all peer slots
			for peerNodeID := uint16(1); peerNodeID <= sbdprotocol.SBD_MAX_NODES; peerNodeID++ {
				// Skip our own slot
				if peerNodeID == s.nodeID {
					continue
				}

				if err := s.readPeerHeartbeat(peerNodeID); err != nil {
					logger.Info("Error reading peer heartbeat", "peerNodeID", peerNodeID, "error", err)
					// Continue with other peers even if one fails
				}
			}

			// Check liveness of all tracked peers
			s.peerMonitor.CheckPeerLiveness()

			// Log cluster status periodically
			healthyPeers := s.peerMonitor.GetHealthyPeerCount()
			logger.Info("Cluster status", "healthyPeers", healthyPeers)
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
		logger.Info("WARNING: SBD device is not a device file", "devicePath", devicePath)
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

// getRebootMethodFromEnv gets the reboot method from environment variables if not provided via flag
func getRebootMethodFromEnv() string {
	envVars := []string{"SBD_REBOOT_METHOD", "REBOOT_METHOD"}

	for _, envVar := range envVars {
		if value := os.Getenv(envVar); value != "" {
			if value == "panic" || value == "systemctl-reboot" {
				return value
			}
		}
	}

	return "panic" // Default method
}

// isSelfFenceDetected checks if a self-fence has been detected
func (s *SBDAgent) isSelfFenceDetected() bool {
	s.selfFenceMutex.RLock()
	defer s.selfFenceMutex.RUnlock()
	return s.selfFenceDetected
}

// setSelfFenceDetected sets the self-fence detected flag
func (s *SBDAgent) setSelfFenceDetected(detected bool) {
	s.selfFenceMutex.Lock()
	defer s.selfFenceMutex.Unlock()
	s.selfFenceDetected = detected
}

// executeSelfFencing performs the self-fencing action based on the configured method
func (s *SBDAgent) executeSelfFencing(reason string) {
	logger.Error(nil, "Self-fencing initiated",
		"reason", reason,
		"rebootMethod", s.rebootMethod,
		"nodeID", s.nodeID,
		"nodeName", s.nodeName)

	// Increment self-fenced counter
	selfFencedCounter.Inc()
	// Mark agent as unhealthy
	agentHealthyGauge.Set(0)

	// Mark self-fence as detected to stop watchdog petting
	s.setSelfFenceDetected(true)

	// Give some time for the log message to be written
	time.Sleep(100 * time.Millisecond)

	switch s.rebootMethod {
	case "systemctl-reboot":
		logger.Error(nil, "Attempting systemctl reboot for self-fencing",
			"reason", reason,
			"nodeID", s.nodeID)
		if err := exec.Command("sudo", "systemctl", "reboot").Run(); err != nil {
			logger.Error(err, "Failed to execute systemctl reboot, falling back to panic",
				"reason", reason,
				"nodeID", s.nodeID)
			panic(fmt.Sprintf("Self-fencing via systemctl failed: %v", err))
		}
	case "panic":
		fallthrough
	default:
		logger.Error(nil, "Initiating panic for immediate self-fencing",
			"reason", reason,
			"nodeID", s.nodeID,
			"nodeName", s.nodeName)
		panic(fmt.Sprintf("Self-fencing: %s", reason))
	}
}

// readOwnSlotForFenceMessage reads the agent's own slot to check for fence messages
func (s *SBDAgent) readOwnSlotForFenceMessage() error {
	if s.sbdDevice == nil || s.sbdDevice.IsClosed() {
		return fmt.Errorf("SBD device is not available")
	}

	// Calculate slot offset for our own node
	slotOffset := int64(s.nodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Read the entire slot
	slotData := make([]byte, sbdprotocol.SBD_SLOT_SIZE)
	n, err := s.sbdDevice.ReadAt(slotData, slotOffset)
	if err != nil {
		return fmt.Errorf("failed to read own slot %d from offset %d: %w", s.nodeID, slotOffset, err)
	}

	if n != sbdprotocol.SBD_SLOT_SIZE {
		return fmt.Errorf("partial read from own slot %d: read %d bytes, expected %d", s.nodeID, n, sbdprotocol.SBD_SLOT_SIZE)
	}

	// Try to unmarshal the message header
	header, err := sbdprotocol.Unmarshal(slotData[:sbdprotocol.SBD_HEADER_SIZE])
	if err != nil {
		// Not a valid message, could be empty slot or heartbeat we wrote
		logger.V(1).Info("Failed to unmarshal message from own slot",
			"nodeID", s.nodeID,
			"error", err)
		return nil
	}

	// Check if this is a fence message
	if header.Type == sbdprotocol.SBD_MSG_TYPE_FENCE {
		// Try to unmarshal as a fence message to get the target
		fenceMsg, err := sbdprotocol.UnmarshalFence(slotData[:sbdprotocol.SBD_HEADER_SIZE+3])
		if err != nil {
			logger.Error(err, "Failed to unmarshal fence message from own slot",
				"nodeID", s.nodeID)
			return nil
		}

		// Check if this fence message is directed at us
		if fenceMsg.TargetNodeID == s.nodeID {
			reason := fmt.Sprintf("Fence message received from node %d, reason: %s",
				fenceMsg.Header.NodeID, sbdprotocol.GetFenceReasonName(fenceMsg.Reason))
			logger.Error(nil, "Fence message detected in own slot",
				"reason", reason,
				"sourceNodeID", fenceMsg.Header.NodeID,
				"targetNodeID", fenceMsg.TargetNodeID,
				"fenceReason", sbdprotocol.GetFenceReasonName(fenceMsg.Reason))

			// Execute self-fencing immediately
			s.executeSelfFencing(reason)
		} else {
			logger.V(1).Info("Fence message in own slot not directed at us",
				"targetNodeID", fenceMsg.TargetNodeID,
				"ourNodeID", s.nodeID,
				"sourceNodeID", fenceMsg.Header.NodeID)
		}
	}

	return nil
}

func main() {
	flag.Parse()

	// Initialize structured logger first
	if err := initializeLogger(*logLevel); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	logger.Info("SBD Agent starting", "version", "development")

	// Determine node name
	nodeNameValue := *nodeName
	if nodeNameValue == "" {
		nodeNameValue = getNodeNameFromEnv()
		if nodeNameValue == "" {
			logger.Error(nil, "Node name must be specified via --node-name flag or NODE_NAME environment variable")
			os.Exit(1)
		}
		logger.Info("Using node name from environment", "nodeName", nodeNameValue)
	}

	// Validate node name length
	if len(nodeNameValue) > MaxNodeNameLength {
		logger.Error(nil, "Node name too long",
			"nodeNameLength", len(nodeNameValue),
			"maxLength", MaxNodeNameLength,
			"nodeName", nodeNameValue)
		os.Exit(1)
	}

	// Determine node ID
	nodeIDValue := uint16(*nodeID)
	if nodeIDValue == 0 {
		nodeIDValue = getNodeIDFromEnv()
		logger.Info("Using node ID from environment or default", "nodeID", nodeIDValue)
	}

	// Validate node ID
	if nodeIDValue < 1 || nodeIDValue > sbdprotocol.SBD_MAX_NODES {
		logger.Error(nil, "Invalid node ID",
			"nodeID", nodeIDValue,
			"minNodeID", 1,
			"maxNodeID", sbdprotocol.SBD_MAX_NODES)
		os.Exit(1)
	}

	// Determine SBD timeout
	sbdTimeoutValue := *sbdTimeoutSeconds
	if sbdTimeoutValue == 30 { // Check if it's still the default
		sbdTimeoutValue = getSBDTimeoutFromEnv()
		logger.Info("Using SBD timeout from environment or default",
			"sbdTimeoutSeconds", sbdTimeoutValue)
	}

	// Determine reboot method
	rebootMethodValue := *rebootMethod
	if rebootMethodValue == "panic" { // Check if it's still the default
		rebootMethodValue = getRebootMethodFromEnv()
		logger.Info("Using reboot method from environment or default",
			"rebootMethod", rebootMethodValue)
	}

	// Validate reboot method
	if rebootMethodValue != "panic" && rebootMethodValue != "systemctl-reboot" {
		logger.Error(nil, "Invalid reboot method",
			"rebootMethod", rebootMethodValue,
			"validMethods", []string{"panic", "systemctl-reboot"})
		os.Exit(1)
	}

	// Calculate heartbeat interval (sbdTimeoutSeconds / 2)
	heartbeatInterval := time.Duration(sbdTimeoutValue/2) * time.Second
	if heartbeatInterval < time.Second {
		heartbeatInterval = time.Second // Minimum 1 second interval
	}

	// Validate required parameters
	if *sbdDevice == "" {
		logger.Error(nil, "No SBD device specified, running in watchdog-only mode")
	} else {
		if err := validateSBDDevice(*sbdDevice); err != nil {
			logger.Error(err, "SBD device validation failed", "sbdDevice", *sbdDevice)
			os.Exit(1)
		}
	}

	// Create SBD agent
	agent, err := NewSBDAgent(*watchdogPath, *sbdDevice, nodeNameValue, nodeIDValue, *watchdogTimeout, *sbdUpdateInterval, heartbeatInterval, *peerCheckInterval, sbdTimeoutValue, rebootMethodValue, *metricsPort)
	if err != nil {
		logger.Error(err, "Failed to create SBD agent",
			"watchdogPath", *watchdogPath,
			"sbdDevice", *sbdDevice,
			"nodeName", nodeNameValue,
			"nodeID", nodeIDValue)
		os.Exit(1)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the agent
	if err := agent.Start(); err != nil {
		logger.Error(err, "Failed to start SBD agent")
		os.Exit(1)
	}

	// Wait for shutdown signal
	sig := <-sigChan
	logger.Info("Received shutdown signal", "signal", sig.String())

	// Stop the agent
	if err := agent.Stop(); err != nil {
		logger.Error(err, "Error during shutdown")
	}

	logger.Info("SBD Agent shutdown complete")
}
