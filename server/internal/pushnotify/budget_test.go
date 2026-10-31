package pushnotify

import "testing"

// CHE-1795: same-device sends are serialized by the device lock, so
// the worst case for one claimed batch is every row on one device, each send
// taking the full sendTimeout. The lease has to cover that, otherwise rows
// queued at the back of the batch outlive their claim.
func TestWorstCaseSameDeviceBatchFitsTheLease(t *testing.T) {
	if worst := batchSize * sendTimeout; worst >= leaseDuration {
		t.Fatalf("a batch of %d sends of up to %v takes %v, which is not under the %v lease", batchSize, sendTimeout, worst, leaseDuration)
	}
}
