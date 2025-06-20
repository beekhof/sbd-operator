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
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/medik8s/sbd-operator/pkg/watchdog"
)

var (
	watchdogPath    = flag.String("watchdog-path", "/dev/watchdog", "Path to the watchdog device")
	watchdogTimeout = flag.Duration("watchdog-timeout", 30*time.Second, "Watchdog pet interval")
	sbdDevice       = flag.String("sbd-device", "", "Path to the SBD block device")
	logLevel        = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

// SBDAgent represents the main SBD agent application
type SBDAgent struct {
	watchdog      *watchdog.Watchdog
	sbdDevicePath string
	petInterval   time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
}

// NewSBDAgent creates a new SBD agent instance
func NewSBDAgent(watchdogPath, sbdDevicePath string, petInterval time.Duration) (*SBDAgent, error) {
	// Initialize watchdog
	wd, err := watchdog.New(watchdogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize watchdog: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &SBDAgent{
		watchdog:      wd,
		sbdDevicePath: sbdDevicePath,
		petInterval:   petInterval,
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// Start begins the SBD agent operations
func (s *SBDAgent) Start() error {
	log.Printf("Starting SBD Agent...")
	log.Printf("Watchdog device: %s", s.watchdog.Path())
	log.Printf("SBD device: %s", s.sbdDevicePath)
	log.Printf("Pet interval: %s", s.petInterval)

	// Start the watchdog monitoring goroutine
	go s.watchdogLoop()

	// TODO: Add SBD device monitoring and message handling
	// This would include:
	// - Reading SBD messages from the block device
	// - Processing fence requests
	// - Updating node status

	log.Printf("SBD Agent started successfully")
	return nil
}

// Stop gracefully shuts down the SBD agent
func (s *SBDAgent) Stop() error {
	log.Printf("Stopping SBD Agent...")

	// Cancel context to stop all goroutines
	s.cancel()

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
			if err := s.watchdog.Pet(); err != nil {
				log.Printf("ERROR: Failed to pet watchdog: %v", err)
				// Continue trying - don't exit on pet failure
			} else {
				log.Printf("DEBUG: Watchdog pet successful")
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

func main() {
	flag.Parse()

	log.Printf("SBD Agent starting...")
	log.Printf("Version: development")

	// Validate required parameters
	if *sbdDevice == "" {
		log.Printf("WARNING: No SBD device specified, running in watchdog-only mode")
	} else {
		if err := validateSBDDevice(*sbdDevice); err != nil {
			log.Fatalf("SBD device validation failed: %v", err)
		}
	}

	// Create SBD agent
	agent, err := NewSBDAgent(*watchdogPath, *sbdDevice, *watchdogTimeout)
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
