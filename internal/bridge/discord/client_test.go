package discord

import "testing"

// TestFakeClientSatisfiesClient is a compile-time assertion with a runtime
// home, so the failure is a test failure rather than a confusing error in
// an unrelated file.
func TestFakeClientSatisfiesClient(t *testing.T) {
	var _ Client = newFakeClient()
}

// TestGatewayClientSatisfiesClient pins the "no dial on construction"
// contract: NewGatewayClient must not dial, because it is called during
// daemon startup and a network round trip there would make startup fail on
// a flaky link rather than on a bad token. A bogus token proves it — if
// NewGatewayClient ever tried to reach Discord, this test would hang or
// fail on the network rather than returning immediately.
func TestGatewayClientSatisfiesClient(t *testing.T) {
	var _ Client = (*gatewayClient)(nil)

	c, err := NewGatewayClient("not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}
