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
	heartbeat2 := sbdprotocol.SBDHeartbeatMessage{
		Header: sbdprotocol.NewHeartbeat(99, "very-long-node-name-for-testing", 456),
	}

	msgBytes2, err := sbdprotocol.MarshalHeartbeat(heartbeat2)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	t.Logf("Long name 'very-long-node-name-for-testing': size=%d", len(msgBytes2))

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
