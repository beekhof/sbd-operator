package main

import (
	"testing"

	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
)

func TestDebugMessageSizes(t *testing.T) {
	// Test heartbeat with short name
	heartbeat1 := sbdprotocol.SBDHeartbeatMessage{
		Header: sbdprotocol.NewHeartbeat(42, "node1", 123),
	}

	msgBytes1, err := sbdprotocol.MarshalHeartbeat(heartbeat1)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	t.Logf("Short name 'node1': size=%d, data=%x", len(msgBytes1), msgBytes1)

	// Test heartbeat with long name
	longNodeName := "very-long-node-name-for-testing-variable-length-parsing"
	heartbeat2 := sbdprotocol.SBDHeartbeatMessage{
		Header: sbdprotocol.NewHeartbeat(99, longNodeName, 456),
	}

	msgBytes2, err := sbdprotocol.MarshalHeartbeat(heartbeat2)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	t.Logf("Long name '%s': length=%d, size=%d", longNodeName, len(longNodeName), len(msgBytes2))

	// Test extraction
	slot := make([]byte, sbdprotocol.SBD_SLOT_SIZE)
	copy(slot, msgBytes2)

	extracted, err := extractMessageFromSlot(slot)
	if err != nil {
		t.Fatalf("Failed to extract: %v", err)
	}

	t.Logf("Extracted size: %d, expected: %d", len(extracted), 34+len(longNodeName))

	// Test fence message
	fence := sbdprotocol.SBDFenceMessage{
		Header:       sbdprotocol.NewFence(1, "fencer", 2, 789, sbdprotocol.FENCE_REASON_MANUAL),
		TargetNodeID: 2,
		Reason:       sbdprotocol.FENCE_REASON_MANUAL,
	}

	fenceBytes, err := sbdprotocol.MarshalFence(fence)
	if err != nil {
		t.Fatalf("Failed to marshal fence: %v", err)
	}

	t.Logf("Fence 'fencer': size=%d", len(fenceBytes))
}
