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
	"sync"
	"syscall"
	"time"

	"github.com/medik8s/sbd-operator/pkg/blockdevice"
	"github.com/medik8s/sbd-operator/pkg/watchdog"
)

var (
	watchdogPath      = flag.String("watchdog-path", "/dev/watchdog", "Path to the watchdog device")
	watchdogTimeout   = flag.Duration("watchdog-timeout", 30*time.Second, "Watchdog pet interval")
	sbdDevice         = flag.String("sbd-device", "", "Path to the SBD block device")
	nodeName          = flag.String("node-name", "", "Name of this Kubernetes node")
	sbdUpdateInterval = flag.Duration("sbd-update-interval", 5*time.Second, "Interval for updating SBD device with node status")
	logLevel          = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

const (
	// SBDNodeIDOffset is the offset where node ID is written in the SBD device
	SBDNodeIDOffset = 0
	// MaxNodeNameLength is the maximum length for a node name in SBD device
	MaxNodeNameLength = 256
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

// SBDAgent represents the main SBD agent application
type SBDAgent struct {
	watchdog          *watchdog.Watchdog
	sbdDevice         BlockDevice
	sbdDevicePath     string
	nodeName          string
	petInterval       time.Duration
	sbdUpdateInterval time.Duration
	ctx               context.Context
	cancel            context.CancelFunc
	sbdHealthy        bool
	sbdHealthyMutex   sync.RWMutex
}

// NewSBDAgent creates a new SBD agent instance
func NewSBDAgent(watchdogPath, sbdDevicePath, nodeName string, petInterval, sbdUpdateInterval time.Duration) (*SBDAgent, error) {
	// Initialize watchdog
	wd, err := watchdog.New(watchdogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize watchdog: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	agent := &SBDAgent{
		watchdog:          wd,
		sbdDevicePath:     sbdDevicePath,
		nodeName:          nodeName,
		petInterval:       petInterval,
		sbdUpdateInterval: sbdUpdateInterval,
		ctx:               ctx,
		cancel:            cancel,
		sbdHealthy:        true, // Start assuming healthy
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

// Start begins the SBD agent operations
func (s *SBDAgent) Start() error {
	log.Printf("Starting SBD Agent...")
	log.Printf("Watchdog device: %s", s.watchdog.Path())
	log.Printf("SBD device: %s", s.sbdDevicePath)
	log.Printf("Node name: %s", s.nodeName)
	log.Printf("Pet interval: %s", s.petInterval)
	log.Printf("SBD update interval: %s", s.sbdUpdateInterval)

	// Start the watchdog monitoring goroutine
	go s.watchdogLoop()

	// Start SBD device monitoring if available
	if s.sbdDevicePath != "" {
		go s.sbdDeviceLoop()
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

	// Validate required parameters
	if *sbdDevice == "" {
		log.Printf("WARNING: No SBD device specified, running in watchdog-only mode")
	} else {
		if err := validateSBDDevice(*sbdDevice); err != nil {
			log.Fatalf("SBD device validation failed: %v", err)
		}
	}

	// Create SBD agent
	agent, err := NewSBDAgent(*watchdogPath, *sbdDevice, nodeNameValue, *watchdogTimeout, *sbdUpdateInterval)
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
